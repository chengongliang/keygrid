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
