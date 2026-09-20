package relay

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
)

// route.go 用户内渠道路由：模型匹配（model_map 反查）、priority 降序 + 同级随机、failover 候选序列。

// candidate 一个可用渠道候选。
type candidate struct {
	provider *model.Provider
	upModel  string // model_map 反查后的上游模型名
}

// buildCandidates 返回支持该模型的所有渠道，按 priority 降序、同级随机洗牌（权重化 failover 顺序）。
// 有 model_map 的渠道只支持 map 里的模型；无 map 的渠道透传任意模型。
// allowProviders 非空时为 key 级渠道白名单：只保留白名单内的渠道（nil/空 = 不限）。
func buildCandidates(providers []model.Provider, modelName string, allowProviders []int64) []candidate {
	var allow map[int64]bool
	if len(allowProviders) > 0 {
		allow = make(map[int64]bool, len(allowProviders))
		for _, id := range allowProviders {
			allow[id] = true
		}
	}
	byPriority := map[int][]candidate{}
	for i := range providers {
		p := &providers[i]
		// kind=oauth 渠道参与路由（凭据在 relay.credentialSecret 解密/刷新）
		if !p.Enabled || (p.Kind != "api_key" && p.Kind != "oauth") {
			continue
		}
		if allow != nil && !allow[p.ID] {
			continue
		}
		if len(p.ModelMap) > 0 {
			up, ok := p.ModelMap[modelName]
			if !ok {
				continue
			}
			byPriority[p.Priority] = append(byPriority[p.Priority], candidate{provider: p, upModel: up})
		} else {
			byPriority[p.Priority] = append(byPriority[p.Priority], candidate{provider: p, upModel: modelName})
		}
	}
	if len(byPriority) == 0 {
		return nil
	}

	// priority 值降序排列
	priorities := make([]int, 0, len(byPriority))
	for pr := range byPriority {
		priorities = append(priorities, pr)
	}
	for i := 0; i < len(priorities); i++ {
		for j := i + 1; j < len(priorities); j++ {
			if priorities[j] > priorities[i] {
				priorities[i], priorities[j] = priorities[j], priorities[i]
			}
		}
	}

	var cands []candidate
	for _, pr := range priorities {
		list := byPriority[pr]
		rand.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
		cands = append(cands, list...)
	}
	return cands
}

// clientGone 判断是否客户端断开（failover 停止条件之一）。
func clientGone(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled"))
}

// upstreamTarget 构建上游 chat/completions URL。
func upstreamTarget(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
}

// shouldFailover 判定上游结果是否值得 failover（网络错误 / 429 / 5xx）。
func shouldFailover(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == http.StatusTooManyRequests || status >= 500
}

// breakerCheckEnabled 该渠道是否参与熔断检测（providers.breaker_check，默认关）。
//
// 熔断的收益是“别把请求继续浪费在已知坏掉的渠道上”，而这需要旁边有备选渠道
// 可以 failover。只有一条渠道、上游本就不稳的用户，熔断的代价是整段不可用，
// 比一直重试更糟 —— 所以默认关闭，由用户按需开启。
func breakerCheckEnabled(p *model.Provider) bool {
	return p != nil && p.BreakerCheck
}

// channelFailure 判定一次上游调用的失败是否应计入渠道熔断。
//
// 只有“渠道 / 网络层面”的故障才计入：
//   - 传输层错误（连接失败、超时、上游半路中断读流）；
//   - 5xx（上游自身故障）；
//   - 429（上游限速：连续命中说明配额已耗尽，先熔断止损）。
//
// 4xx（400 参数错/上下文超长、401/403 凭据无权、404 模型不存在、413 体过大…）
// 是请求或凭据层面的问题，渠道本身仍然健康：若计入熔断，一个被客户端反复重试
// 的坏请求会在几秒内把整条渠道（连同该渠道上的其它模型）打成不可用，用户看到
// 的就是 “circuit open”。这类错误照常 failover 到下一个候选渠道，但渠道健康
// 必须由成功的请求来证明，而不是由失败的请求来判定。
func channelFailure(err error, status int) bool {
	if err == nil {
		return shouldFailover(status, nil)
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == http.StatusTooManyRequests || ue.Status >= 500
	}
	// 非 UpstreamError：传输层错误（连接/超时/中断读），按故障计
	return true
}
