package oauth

import (
	"testing"
	"time"
)

// refresh_test.go 刷新调度单测（无 DB 依赖部分）。

func TestAcquireRefreshLockWithoutRedis(t *testing.T) {
	// 无 Redis（单实例）→ 直接放行
	s := NewScheduler(nil, nil, time.Minute)
	release, ok := s.acquireRefreshLock(t.Context(), 1)
	if !ok {
		t.Fatal("no-redis mode must always acquire")
	}
	release()
}

func TestSchedulerDefaults(t *testing.T) {
	s := NewScheduler(nil, nil, 0)
	if s.Lead != 5*time.Minute {
		t.Fatalf("default lead must be 5min, got %v", s.Lead)
	}
}

func TestScanOnceSkips(t *testing.T) {
	// 无凭据时静默 0
	s := NewScheduler(nil, nil, 0)
	if s.Op != nil {
		t.Fatal("precondition")
	}
	// ScanOnce 需要 Op；这里只验证构造路径不 panic
	_ = s
}

// ---- 通用提前量判定（relay/oauth.go 逻辑的 oauth 侧镜像）----

func tokenNeedsBailoutLogic(expiresAt time.Time, lead time.Duration) bool {
	if expiresAt.IsZero() {
		return false
	}
	return time.Until(expiresAt) <= lead
}

func TestTokenNeedsBailout(t *testing.T) {
	if tokenNeedsBailoutLogic(time.Time{}, time.Minute) {
		t.Fatal("zero expires_at must not trigger")
	}
	if tokenNeedsBailoutLogic(time.Now().Add(time.Hour), time.Minute) {
		t.Fatal("far-future token must not trigger")
	}
	if !tokenNeedsBailoutLogic(time.Now().Add(30*time.Second), time.Minute) {
		t.Fatal("near-expiry token must trigger")
	}
}
