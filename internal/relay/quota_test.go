package relay

import (
	"context"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// fakeQuotaStore quota 回填读取 fake。
type fakeQuotaStore struct {
	key *model.ApiKey
	err error
}

func (f *fakeQuotaStore) GetApiKeyGlobal(id int64) (*model.ApiKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.key, nil
}

// 无 Redis（rdb=nil）：全程走 DB 降级路径 —— Redis 故障场景的核心语义。
func TestQuotaOverLimitDBFallback(t *testing.T) {
	store := &fakeQuotaStore{key: &model.ApiKey{ID: 7, QuotaLimit: 10, QuotaUsed: 9.99}}
	q := NewQuotaEnforcerWithStore(nil, store, true)

	key := &model.ApiKey{ID: 7, QuotaLimit: 10}
	// used=9.99 < limit=10 → 放行
	if _, _, over := q.OverLimit(context.Background(), key); over {
		t.Fatalf("used 9.99 < limit 10 should not be over")
	}
	// used=10 >= limit → 超限
	store.key.QuotaUsed = 10
	if _, _, over := q.OverLimit(context.Background(), key); !over {
		t.Fatalf("used 10 >= limit 10 should be over")
	}
	store.key.QuotaUsed = 12.5
	if used, _, over := q.OverLimit(context.Background(), key); !over || used != 12.5 {
		t.Fatalf("used 12.5 should be over, got used=%v over=%v", used, over)
	}
}

func TestQuotaUnlimitedKey(t *testing.T) {
	store := &fakeQuotaStore{key: &model.ApiKey{ID: 7, QuotaUsed: 999}}
	q := NewQuotaEnforcerWithStore(nil, store, true)
	// limit=0（不限）恒放行，且不应读 DB
	if _, limit, over := q.OverLimit(context.Background(), &model.ApiKey{ID: 7}); over || limit != 0 {
		t.Fatalf("unlimited key should never be over, limit=%v over=%v", limit, over)
	}
}

func TestQuotaDisabledGlobally(t *testing.T) {
	store := &fakeQuotaStore{key: &model.ApiKey{ID: 7, QuotaLimit: 10, QuotaUsed: 99}}
	q := NewQuotaEnforcerWithStore(nil, store, false) // QUOTA_ENFORCE=0：纯统计模式
	if _, _, over := q.OverLimit(context.Background(), &model.ApiKey{ID: 7, QuotaLimit: 10}); over {
		t.Fatalf("globally disabled should not block")
	}
	// 关闭时 Add/Reset 静默跳过（nil rdb + Enabled=false 不 panic）
	q.Add(context.Background(), 7, 1.5)
	q.Reset(context.Background(), 7)
}

func TestQuotaDBErrorFailsOpen(t *testing.T) {
	store := &fakeQuotaStore{err: context.DeadlineExceeded}
	q := NewQuotaEnforcerWithStore(nil, store, true)
	// DB 也读不到：放行（鉴权刚成功说明 DB 活着，短暂抖动不挡死转发面；打日志）
	_, _, over := q.OverLimit(context.Background(), &model.ApiKey{ID: 7, QuotaLimit: 10})
	if over {
		t.Fatalf("storage error should fail-open, not block")
	}
}

func TestQuotaNilEnforcer(t *testing.T) {
	var q *QuotaEnforcer
	// nil 检查器/nil Add 不 panic（转发路径无需判空）
	if _, _, over := q.OverLimit(context.Background(), &model.ApiKey{ID: 1, QuotaLimit: 10}); over {
		t.Fatalf("nil enforcer should not block")
	}
	q.Add(context.Background(), 1, 1)
	q.Reset(context.Background(), 1)
}
