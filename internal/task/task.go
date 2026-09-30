package task

import (
	"context"
	"log"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/workbuddy"
)

// task.go 后台任务：token 过期扫描刷新 + oauth_states 清理 + 失败诊断过期清理。
// 注：额度不再后台主动探测 —— 只保留用户手动刷新与 relay 被动观察，不对上游做
// 周期性主动轮询。

// requestErrorRetention 失败诊断（request_errors）保留期。
// 只用于排查近期问题，不需要长期归档；过期即整体删除，避免表无上限增长。
const requestErrorRetention = 7 * 24 * time.Hour

// workbuddyJobRunRetention 任务执行日志（workbuddy_job_runs）保留期：
// 面板只展示最近 50 条，30 天足够排查，再老的行没有查询价值。
const workbuddyJobRunRetention = 30 * 24 * time.Hour

// Runner 后台任务集。
type Runner struct {
	Op        *op.Op
	Scheduler *oauth.Scheduler
	// Workbuddy WorkBuddy/CodeBuddy 自动化任务（nil = 不启用）。
	// 调度节奏刻意比 OAuth 扫描慢得多：这些任务在用户账号上模拟真人行为，
	// 高频轮询既无意义又会抬高风控风险。
	Workbuddy *workbuddy.Runtime
	// WorkbuddyInterval 自动化任务扫描间隔；0 = 15 分钟
	WorkbuddyInterval time.Duration

	// Interval 扫描间隔；0 = 60s
	Interval time.Duration

	// lastErrCleanup 上次清理失败诊断的时间（清理每天跑一次即可，
	// 不必跟随 60s 扫描节奏重复 DELETE）
	lastErrCleanup time.Time
	// lastWbTick 上次触发自动化任务的时间（按 WorkbuddyInterval 节流）
	lastWbTick time.Time
	// lastJobRunCleanup 上次清理任务执行日志的时间
	lastJobRunCleanup time.Time
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
	// 自动化任务：总闸关时 Tick 内部直接返回（不做任何用户展开），
	// 所以这里无需先判开关 —— 判断只留一处，避免两处不一致。
	if r.Workbuddy != nil {
		wbInterval := r.WorkbuddyInterval
		if wbInterval <= 0 {
			wbInterval = 15 * time.Minute
		}
		if now := time.Now(); r.lastWbTick.IsZero() || now.Sub(r.lastWbTick) >= wbInterval {
			r.lastWbTick = now
			if n, err := r.Workbuddy.Tick(ctx, now); err != nil {
				log.Printf("[task] workbuddy jobs: %v", err)
			} else if n > 0 {
				log.Printf("[task] workbuddy ran %d job(s)", n)
			}
		}
	}
	// 任务执行日志保留 30 天（面板只展示最近 50 条，更老的无查询价值）
	if now := time.Now(); r.lastJobRunCleanup.IsZero() || now.Sub(r.lastJobRunCleanup) >= 24*time.Hour {
		r.lastJobRunCleanup = now
		if n, err := r.Op.PruneWorkbuddyJobRuns(now.Add(-workbuddyJobRunRetention)); err != nil {
			log.Printf("[task] workbuddy job run cleanup: %v", err)
		} else if n > 0 {
			log.Printf("[task] workbuddy job run cleanup: removed %d row(s)", n)
		}
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
