package task

import (
	"context"
	"log"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
)

// task.go 后台任务：token 过期扫描刷新 + oauth_states 清理 + 失败诊断过期清理。
// 注：额度不再后台主动探测 —— 只保留用户手动刷新与 relay 被动观察，不对上游做
// 周期性主动轮询。

// requestErrorRetention 失败诊断（request_errors）保留期。
// 只用于排查近期问题，不需要长期归档；过期即整体删除，避免表无上限增长。
const requestErrorRetention = 7 * 24 * time.Hour

// Runner 后台任务集。
type Runner struct {
	Op        *op.Op
	Scheduler *oauth.Scheduler
	// Interval 扫描间隔；0 = 60s
	Interval time.Duration

	// lastErrCleanup 上次清理失败诊断的时间（清理每天跑一次即可，
	// 不必跟随 60s 扫描节奏重复 DELETE）
	lastErrCleanup time.Time
}

// Start 启动后台 goroutine（返回停止函数）。
func (r *Runner) Start(ctx context.Context) func() {
	interval := r.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				r.runOnce(ctx)
			}
		}
	}()
	return func() { close(done) }
}

func (r *Runner) runOnce(ctx context.Context) {
	if n, err := r.Scheduler.ScanOnce(ctx); err != nil {
		log.Printf("[task] oauth refresh scan: %v", err)
	} else if n > 0 {
		log.Printf("[task] oauth refreshed %d credential(s)", n)
	}
	if err := r.Op.CleanupExpiredOAuthStates(); err != nil {
		log.Printf("[task] oauth state cleanup: %v", err)
	}
	// SSO 放弃登录会残留 oidc_states，同样定期清理（防无上限增长）
	if err := r.Op.CleanupExpiredOidcStates(); err != nil {
		log.Printf("[task] oidc state cleanup: %v", err)
	}
	// 失败诊断保留 7 天；每天最多清一次（失败量大时表会持续增长，必须回收）
	if now := time.Now(); r.lastErrCleanup.IsZero() || now.Sub(r.lastErrCleanup) >= 24*time.Hour {
		r.lastErrCleanup = now
		if n, err := r.Op.CleanupRequestErrors(now.Add(-requestErrorRetention)); err != nil {
			log.Printf("[task] request error cleanup: %v", err)
		} else if n > 0 {
			log.Printf("[task] request error cleanup: removed %d row(s)", n)
		}
	}
}
