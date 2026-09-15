package relay

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

func TestCostOfExactMatch(t *testing.T) {
	p := NewPricingWithLoader(func() ([]model.ModelPrice, error) {
		return []model.ModelPrice{
			{Model: "gpt-4o", PromptPrice: 2.5, CompletionPrice: 10},
		}, nil
	})

	// gpt-4o: in $2.5/M, out $10/M
	// 1M prompt + 1M completion = 2.5 + 10 = 12.5
	got := p.CostOf("gpt-4o", nil, 1_000_000, 1_000_000)
	if math.Abs(got-12.5) > 1e-9 {
		t.Fatalf("CostOf = %v, want 12.5", got)
	}
	// 小量：1000 tokens prompt = $0.0025
	got = p.CostOf("gpt-4o", nil, 1000, 0)
	if math.Abs(got-0.0025) > 1e-12 {
		t.Fatalf("CostOf = %v, want 0.0025", got)
	}
	// 零 tokens（失败请求）= 0
	if got := p.CostOf("gpt-4o", nil, 0, 0); got != 0 {
		t.Fatalf("zero tokens cost = %v, want 0", got)
	}
}

func TestCostOfWildcardFallback(t *testing.T) {
	p := NewPricingWithLoader(func() ([]model.ModelPrice, error) {
		return []model.ModelPrice{
			{Model: "gpt-4o", PromptPrice: 2.5, CompletionPrice: 10},
			{Model: "*", PromptPrice: 1, CompletionPrice: 2},
		}, nil
	})
	// 未定价模型 → "*" 兜底
	got := p.CostOf("some-unknown-model", nil, 1_000_000, 1_000_000)
	if math.Abs(got-3) > 1e-9 {
		t.Fatalf("wildcard cost = %v, want 3", got)
	}
	// 精确匹配优先于兜底
	got = p.CostOf("gpt-4o", nil, 1_000_000, 0)
	if math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("exact match cost = %v, want 2.5", got)
	}
}

func TestCostOfUnpricedFree(t *testing.T) {
	// 无兜底行：未定价 = 免费（漏配不误拦）
	p := NewPricingWithLoader(func() ([]model.ModelPrice, error) {
		return []model.ModelPrice{
			{Model: "gpt-4o", PromptPrice: 2.5, CompletionPrice: 10},
		}, nil
	})
	if got := p.CostOf("unpriced-model", nil, 1_000_000, 1_000_000); got != 0 {
		t.Fatalf("unpriced cost = %v, want 0", got)
	}
}

func TestPricingLoadFailureKeepsStale(t *testing.T) {
	calls := 0
	p := NewPricingWithLoader(func() ([]model.ModelPrice, error) {
		calls++
		if calls == 1 {
			return []model.ModelPrice{{Model: "gpt-4o", PromptPrice: 2.5, CompletionPrice: 10}}, nil
		}
		return nil, errors.New("db down")
	})
	// 首次加载成功
	if got := p.CostOf("gpt-4o", nil, 1_000_000, 0); math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("cost = %v, want 2.5", got)
	}
	// 推进时间触发刷新；刷新失败但旧缓存仍可用（stale，不阻塞转发）
	p.Now = func() time.Time { return time.Now().Add(2 * pricingTTL) }
	if got := p.CostOf("gpt-4o", nil, 1_000_000, 0); math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("stale cost = %v, want 2.5", got)
	}
}

func TestPricingTTLRefresh(t *testing.T) {
	now := time.Now()
	prices := []model.ModelPrice{{Model: "gpt-4o", PromptPrice: 2.5, CompletionPrice: 10}}
	p := NewPricingWithLoader(func() ([]model.ModelPrice, error) { return prices, nil })
	p.Now = func() time.Time { return now }

	_ = p.CostOf("gpt-4o", nil, 1, 1)
	// TTL 内：缓存生效，价格变更不可见
	prices[0].PromptPrice = 99
	_ = p.CostOf("gpt-4o", nil, 1_000_000, 0)
	if got := p.CostOf("gpt-4o", nil, 1_000_000, 0); math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("TTL 内应读缓存，cost = %v, want 2.5", got)
	}
	// TTL 过期：重新加载，新价生效
	p.Now = func() time.Time { return now.Add(2 * pricingTTL) }
	if got := p.CostOf("gpt-4o", nil, 1_000_000, 0); math.Abs(got-99) > 1e-9 {
		t.Fatalf("TTL 过期应刷新，cost = %v, want 99", got)
	}
}

// fakeUsageStore 捕获 flush 调用（quota 增量聚合验证用）。
type fakeUsageStore struct {
	logs   []*model.UsageLog
	deltas map[int64]float64
	calls  int
}

func (f *fakeUsageStore) FlushUsageBatch(logs []*model.UsageLog, deltas map[int64]float64) error {
	f.logs = append(f.logs, logs...)
	if f.deltas == nil {
		f.deltas = map[int64]float64{}
	}
	for k, v := range deltas {
		f.deltas[k] += v
	}
	f.calls++
	return nil
}

func TestUsageWriterFlushQuotaDeltas(t *testing.T) {
	store := &fakeUsageStore{}
	uw := NewUsageWriter(store, 100, time.Hour) // 长周期：flush 只由 Stop/close 触发
	uw.Record(usageEntry{userID: 1, apiKeyID: 7, model: "gpt-4o", promptTokens: 1000, cost: 0.0025})
	uw.Record(usageEntry{userID: 1, apiKeyID: 7, model: "gpt-4o", promptTokens: 2000, cost: 0.005})
	uw.Record(usageEntry{userID: 2, apiKeyID: 9, model: "gpt-4o", promptTokens: 100, cost: 0.00025})
	// 失败请求：cost=0，不应产生 delta
	uw.Record(usageEntry{userID: 2, apiKeyID: 9, model: "gpt-4o", statusCode: 500})
	uw.Stop() // close(ch) → flush 剩余

	if len(store.logs) != 4 {
		t.Fatalf("flushed logs = %d, want 4", len(store.logs))
	}
	if len(store.deltas) != 2 {
		t.Fatalf("delta keys = %d, want 2", len(store.deltas))
	}
	if math.Abs(store.deltas[7]-0.0075) > 1e-12 {
		t.Fatalf("delta[7] = %v, want 0.0075", store.deltas[7])
	}
	if math.Abs(store.deltas[9]-0.00025) > 1e-12 {
		t.Fatalf("delta[9] = %v, want 0.00025", store.deltas[9])
	}
}

func TestCostOfBillingMap(t *testing.T) {
	prices := []model.ModelPrice{
		{Model: "deepseek-chat", PromptPrice: 0.28, CompletionPrice: 0.42},
		{Model: "gpt-4o", PromptPrice: 2.5, CompletionPrice: 10},
	}
	p := NewPricingWithLoader(func() ([]model.ModelPrice, error) { return prices, nil })

	chanA := &model.Provider{ID: 1, BillingMap: map[string]string{"my-alias": "deepseek-chat"}}
	chanB := &model.Provider{ID: 2, BillingMap: map[string]string{"my-alias": "gpt-4o"}} // 同名异价：渠道级可表达

	// 渠道 A：my-alias → deepseek-chat（$0.28/M prompt）
	got := p.CostOf("my-alias", chanA, 1_000_000, 0)
	if math.Abs(got-0.28) > 1e-9 {
		t.Fatalf("chanA cost = %v, want 0.28", got)
	}
	// 渠道 B：同一请求名映射到不同标准模型（$2.5/M prompt）
	got = p.CostOf("my-alias", chanB, 1_000_000, 0)
	if math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("chanB cost = %v, want 2.5", got)
	}
	// 入口名已命中价格表时 BillingMap 不生效（精确优先，防绕过）
	chanTrap := &model.Provider{ID: 3, BillingMap: map[string]string{"gpt-4o": "deepseek-chat"}}
	got = p.CostOf("gpt-4o", chanTrap, 1_000_000, 0)
	if math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("exact-match priority: cost = %v, want 2.5", got)
	}
	// 映射目标不在价格表 → 兜底/免费（不按映射名计费）
	chanBad := &model.Provider{ID: 4, BillingMap: map[string]string{"x": "not-priced"}}
	if got := p.CostOf("x", chanBad, 1_000_000, 0); got != 0 {
		t.Fatalf("bad target cost = %v, want 0", got)
	}
	// billing 为 nil（无渠道上下文）不 panic
	if got := p.CostOf("my-alias", nil, 1_000_000, 0); got != 0 {
		t.Fatalf("nil provider cost = %v, want 0", got)
	}
	// 映射到 "*" 被忽略（兜底价不可作为映射目标）
	chanStar := &model.Provider{ID: 5, BillingMap: map[string]string{"x": "*"}}
	if got := p.CostOf("x", chanStar, 1_000_000, 0); got != 0 {
		t.Fatalf("star target cost = %v, want 0", got)
	}
}

func TestBillingNameOf(t *testing.T) {
	prices := []model.ModelPrice{
		{Model: "deepseek-chat", PromptPrice: 0.28, CompletionPrice: 0.42},
	}
	p := NewPricingWithLoader(func() ([]model.ModelPrice, error) { return prices, nil })
	prov := &model.Provider{ID: 1, BillingMap: map[string]string{"my-alias": "deepseek-chat"}}

	// 归一化生效 → 返回标准名
	if got := p.BillingNameOf("my-alias", prov); got != "deepseek-chat" {
		t.Fatalf("BillingNameOf = %q, want deepseek-chat", got)
	}
	// 已命中价格表 → 空串（聚合按 model 即可）
	if got := p.BillingNameOf("deepseek-chat", prov); got != "" {
		t.Fatalf("BillingNameOf exact = %q, want empty", got)
	}
	// 无映射 → 空串
	if got := p.BillingNameOf("my-alias", nil); got != "" {
		t.Fatalf("BillingNameOf nil = %q, want empty", got)
	}
}
