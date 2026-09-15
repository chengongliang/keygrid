package relay

import (
	"log"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
)

// pricing.go 模型价格内存缓存（30s TTL）：admin 全局价格表 → 转发热路径按模型算 cost，
// 零 DB 查询；admin 改价后最多 30s 生效。拉取失败时保留旧缓存（stale 可用），避免价格源
// 故障影响转发；首次拉取失败视为全表未定价（cost=0），同样不阻塞转发。
// 匹配规则：精确模型名 > "*" 兜底行 > 未定价（cost=0，免费且不计入额度累计）。

const pricingTTL = 30 * time.Second

// Pricing 价格缓存。
type Pricing struct {
	load func() ([]model.ModelPrice, error) // 价格表加载源（生产绑 op.ListModelPrices；测试可注入）

	mu       sync.RWMutex
	byModel  map[string]model.ModelPrice // 精确模型名 → 条目
	wildcard *model.ModelPrice           // "*" 兜底行（无则 nil）
	loadedAt time.Time
	loaded   bool

	// 测试注入
	Now func() time.Time
}

func NewPricing(o *op.Op) *Pricing {
	return &Pricing{load: o.ListModelPrices, Now: time.Now}
}

// NewPricingWithLoader 自定义加载源（测试注入用）。
func NewPricingWithLoader(load func() ([]model.ModelPrice, error)) *Pricing {
	return &Pricing{load: load, Now: time.Now}
}

// resolve 计费解析：返回实际计费条目与计费名。
// 匹配链：价格表精确匹配入口名 → 未命中且渠道有 BillingMap → 归一化后再精确匹配
// → "*" 兜底 → nil（未定价 = 免费，且不计入额度累计）。
// 入口名已命中价格表时 BillingMap 不生效（精确优先，防映射绕过）；billing 可为 nil（无渠道上下文）。
func (p *Pricing) resolve(modelName string, billing *model.Provider) (*model.ModelPrice, string) {
	if pr := p.lookup(modelName); pr != nil {
		return pr, modelName
	}
	if billing != nil {
		// 渠道级计费名归一化：my-alias → deepseek-chat（按实际成功渠道映射，同名异价可表达）
		if std, ok := billing.BillingMap[modelName]; ok && std != "" && std != "*" {
			if pr := p.lookup(std); pr != nil {
				return pr, std
			}
		}
	}
	return nil, modelName
}

// CostOf 转发热路径计费入口：按入口模型名查价算费用（USD）；未定价返回 0。
// 失败请求 tokens=0 时由调用方保证结果也为 0。
func (p *Pricing) CostOf(modelName string, billing *model.Provider, promptTokens, completionTokens int) float64 {
	pr, _ := p.resolve(modelName, billing)
	if pr == nil {
		return 0
	}
	return float64(promptTokens)/1e6*pr.PromptPrice +
		float64(completionTokens)/1e6*pr.CompletionPrice
}

// BillingNameOf 归一化计费名（usage_logs.billing_model 用）：
// 实际计费名 ≠ 入口名时返回标准名（如 "deepseek-chat"），否则返回空串（聚合按 model 即可）。
func (p *Pricing) BillingNameOf(modelName string, billing *model.Provider) string {
	_, name := p.resolve(modelName, billing)
	if name == modelName {
		return ""
	}
	return name
}

// lookup 带双检的缓存读取 + TTL 过期回源。
func (p *Pricing) lookup(modelName string) *model.ModelPrice {
	p.mu.RLock()
	if p.loaded && p.Now().Sub(p.loadedAt) < pricingTTL {
		pr := p.lookupLocked(modelName)
		p.mu.RUnlock()
		return pr
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	// double-check：读锁释放后可能已被其他 goroutine 刷新
	if p.loaded && p.Now().Sub(p.loadedAt) < pricingTTL {
		return p.lookupLocked(modelName)
	}

	ps, err := p.load()
	if err != nil {
		// 保留旧缓存（可能为空 = 未定价），下个 TTL 再试；打日志不阻塞转发
		log.Printf("[pricing] refresh failed, keep stale cache: %v", err)
		p.loadedAt = p.Now()
		p.loaded = true
		return p.lookupLocked(modelName)
	}
	byModel := make(map[string]model.ModelPrice, len(ps))
	var wildcard *model.ModelPrice
	for i := range ps {
		if ps[i].Model == "*" {
			w := ps[i]
			wildcard = &w
			continue
		}
		byModel[ps[i].Model] = ps[i]
	}
	p.byModel = byModel
	p.wildcard = wildcard
	p.loadedAt = p.Now()
	p.loaded = true
	return p.lookupLocked(modelName)
}

// lookupLocked 精确匹配 → "*" 兜底 → nil（未定价）。调用方需持锁。
func (p *Pricing) lookupLocked(modelName string) *model.ModelPrice {
	if pr, ok := p.byModel[modelName]; ok {
		return &pr
	}
	return p.wildcard
}
