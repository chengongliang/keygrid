package task

import (
	"context"
	"log"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
)

// task.go 后台任务：token 过期扫描刷新 + oauth_states 清理。
// 注：额度不再后台主动探测 —— 只保留用户手动刷新与 relay 被动观察，不对上游做
// 周期性主动轮询。

// Runner 后台任务集。
type Runner struct {
	Op        *op.Op
	Scheduler *oauth.Scheduler
	// Interval 扫描间隔；0 = 60s
	Interval time.Duration
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
}
