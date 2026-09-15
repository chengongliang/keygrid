package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // 内嵌时区数据库：alpine 无 tzdata 时 LoadLocation("Asia/Shanghai") 也能用

	"github.com/chengongliang/keygrid/internal/conf"
	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/db"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/router"
	"github.com/chengongliang/keygrid/internal/task"

	// 注册 kimi/openai/anthropic 适配器
	_ "github.com/chengongliang/keygrid/internal/oauth/providers"

	"github.com/redis/go-redis/v9"
)

func main() {
	cfg := conf.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("[conf] %v", err)
	}

	crypto.InitFromSecret(cfg.MasterKey)

	gormDB, err := db.Open(&cfg.Database)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		log.Fatalf("db migrate: %v", err)
	}

	o := &op.Op{DB: gormDB}

	// 首个 admin 提升 —— env ADMIN_EMAIL 存在且用户已注册时置为 admin
	if adminEmail := os.Getenv("ADMIN_EMAIL"); adminEmail != "" {
		if id, err := o.PromoteAdminByEmail(adminEmail); err == nil {
			log.Printf("[admin] promoted %s (id=%d) to admin", adminEmail, id)
		}
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           router.NewWithConfig(o, cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// OAuth 刷新调度（后台扫描 + relay 兜底共用 Scheduler）
	var rdb *redis.Client
	if cfg.RedisAddr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	oauthSched := oauth.NewScheduler(o, rdb, time.Duration(cfg.RefreshLead)*time.Second)
	runner := &task.Runner{
		Op:        o,
		Scheduler: oauthSched,
		Interval:  time.Duration(cfg.OAuthScanInterval) * time.Second,
	}
	stopTask := runner.Start(ctx)
	defer stopTask()

	// Codex 上游模型目录：启动立即远程刷新，之后每 3h 一轮（内嵌快照兑底）
	oauth.StartCodexModelCatalogUpdater(ctx)

	go func() {
		log.Printf("keygrid listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
