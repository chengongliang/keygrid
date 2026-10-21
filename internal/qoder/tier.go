package qoder

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// tier.go Qoder 上下文档位（context tier），移植自 9router
// open-sse/shared/qoder/contextTier.js。
//
// 每个 model_config 自带 context_config（200K / 400K / 1M），而 max_input_tokens
// 只反映 IDE 当前选中的档位（通常 ~180K）。qodercli 形态的客户端没有档位选择器，
// 所以长会话会被上游以“超出上下文”拒绝，尽管模型本身支持 1M。
//
// 这里模拟 IDE：预估 prompt 体量，选“能装下的最小档位”（不低于当前默认档），
// 并同步写入 IDE 会写的三个位置：
//
//	parameters.context_length
//	chat_context.extra.ideModelConfigOverride.max_input_tokens
//	model_config.max_input_tokens
//
// 策略由 QODER_CONTEXT_TIER 控制：auto（默认）| max | default | 具体档位名（如 1M）。

// 档位策略取值。
const (
	TierModeAuto    = "auto"
	TierModeMax     = "max"
	TierModeDefault = "default"
)

// Tier 一个上下文档位。
type Tier struct {
	Name       string
	TokenCount int
	IsDefault  bool
}

// TierChoice 档位决策结果。
type TierChoice struct {
	Tier            Tier
	EstimatedTokens int
	Reason          string
}

// PromptInput 参与体量预估的 prompt 组成。
type PromptInput struct {
	System   string
	Messages any
	Tools    any
}

// parseTierTokenCount "200K" | "1M" | "204800" | 204800 → token 数（无法解析返回 0）。
func parseTierTokenCount(v any) int {
	switch t := v.(type) {
	case float64:
		if t > 0 {
			return int(math.Floor(t))
		}
	case int:
		if t > 0 {
			return t
		}
	case string:
		s := strings.ToUpper(strings.TrimSpace(t))
		if s == "" {
			return 0
		}
		mult := 1.0
		if strings.HasSuffix(s, "K") {
			mult, s = 1000, strings.TrimSuffix(s, "K")
		} else if strings.HasSuffix(s, "M") {
			mult, s = 1000000, strings.TrimSuffix(s, "M")
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || f <= 0 {
			return 0
		}
		return int(math.Floor(f * mult))
	}
	return 0
}

// tierName 档位展示名：优先 name/label/…，否则按 token 数格式化。
func tierName(entry map[string]any, tokenCount int) string {
	for _, k := range []string{"name", "label", "display_name", "displayName", "key", "id"} {
		if v, ok := entry[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if tokenCount >= 1000000 && tokenCount%1000000 == 0 {
		return fmt.Sprintf("%dM", tokenCount/1000000)
	}
	if tokenCount >= 1000 && tokenCount%1000 == 0 {
		return fmt.Sprintf("%dK", tokenCount/1000)
	}
	return strconv.Itoa(tokenCount)
}

// ContextTiers 解析 model_config 的档位列表（按 token 数升序）。
// 兼容 snake_case 与 camelCase 两种形状；没有档位时返回空切片。
func ContextTiers(modelConfig json.RawMessage) []Tier {
	if len(modelConfig) == 0 {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(modelConfig, &cfg); err != nil {
		return nil
	}
	list, ok := cfg["context_config"].([]any)
	if !ok {
		list, ok = cfg["contextConfig"].([]any)
	}
	if !ok {
		return nil
	}

	byCount := map[int]Tier{}
	order := []int{}
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tokenCount := 0
		for _, k := range []string{"tokenCount", "token_count", "max_input_tokens", "maxInputTokens", "contextLength", "context_length"} {
			if tokenCount = parseTierTokenCount(entry[k]); tokenCount > 0 {
				break
			}
		}
		if tokenCount == 0 {
			continue
		}
		isDefault := entry["isDefault"] == true || entry["is_default"] == true || entry["default"] == true
		prev, seen := byCount[tokenCount]
		if !seen {
			order = append(order, tokenCount)
		}
		byCount[tokenCount] = Tier{
			Name:       tierName(entry, tokenCount),
			TokenCount: tokenCount,
			IsDefault:  prev.IsDefault || isDefault,
		}
	}
	if len(order) == 0 {
		return nil
	}
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			if order[j] < order[i] {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	out := make([]Tier, 0, len(order))
	for _, n := range order {
		out = append(out, byCount[n])
	}
	return out
}

// isCJK 中日韩字符（按 1 token/字估算，其余按 4 字符/token）。
func isCJK(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x11ff, // 韩文字母
		r >= 0x2e80 && r <= 0x9fff, // CJK 部首 + 统一表意文字
		r >= 0xac00 && r <= 0xd7af, // 韩文音节
		r >= 0xf900 && r <= 0xfaff, // CJK 兼容表意文字
		r >= 0xff00 && r <= 0xffef: // 全角字符
		return true
	}
	return false
}

// EstimatePromptTokens 粗估 prompt token 数。
// CJK 按 1 字 1 token、其余按 4 字符 1 token —— 纯 chars/4 会把中文低估到 1/4，
// 而档位决策恰恰在这种场景下最敏感。
func EstimatePromptTokens(in PromptInput) int {
	obj := map[string]any{
		"system":   in.System,
		"messages": in.Messages,
		"tools":    in.Tools,
	}
	raw, err := jsonMarshal(obj)
	if err != nil {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range string(raw) {
		if isCJK(r) {
			cjk++
			continue
		}
		other += len(string(r))
	}
	return int(math.Ceil(float64(cjk) + float64(other)/4))
}

// ResolveTier 决定本次请求应使用的档位。
// 返回 nil 表示保持 payload 原样（模型没有档位，或默认档已经装得下）。
func ResolveTier(modelConfig json.RawMessage, prompt PromptInput, preference string) *TierChoice {
	tiers := ContextTiers(modelConfig)
	if len(tiers) == 0 {
		return nil
	}
	mode := strings.TrimSpace(preference)
	if mode == "" {
		mode = TierModeAuto
	}
	largest := tiers[len(tiers)-1]
	defaultTier := tiers[0]
	for _, t := range tiers {
		if t.IsDefault {
			defaultTier = t
			break
		}
	}
	estimated := EstimatePromptTokens(prompt)
	need := int(math.Ceil(float64(estimated) * (1 + ContextTierHeadroom)))

	switch strings.ToLower(mode) {
	case TierModeMax:
		return &TierChoice{Tier: largest, EstimatedTokens: estimated, Reason: "forced:max"}
	case TierModeDefault:
		return &TierChoice{Tier: defaultTier, EstimatedTokens: estimated, Reason: "forced:default"}
	default:
		if named, ok := findNamedTier(tiers, mode); ok {
			return &TierChoice{Tier: named, EstimatedTokens: estimated, Reason: "forced:" + named.Name}
		}
		// 未知档位名 → 回落 auto（而不是让请求失败）
	}

	// auto：默认档装得下就沿用上游当前默认值（行为与之前一致）。
	var cfg map[string]any
	_ = json.Unmarshal(modelConfig, &cfg)
	currentMax := parseTierTokenCount(cfg["max_input_tokens"])
	if currentMax == 0 {
		currentMax = parseTierTokenCount(cfg["maxInputTokens"])
	}
	currentLimit := currentMax
	if currentLimit == 0 {
		currentLimit = defaultTier.TokenCount
	}
	if need <= currentLimit {
		return nil
	}
	var fits *Tier
	for i := range tiers {
		if tiers[i].TokenCount >= need && tiers[i].TokenCount > currentLimit {
			fits = &tiers[i]
			break
		}
	}
	choice := largest
	reason := "auto:largest"
	if fits != nil {
		choice, reason = *fits, "auto:fits"
	}
	if choice.TokenCount <= currentLimit {
		return nil // 没有更大的档位可升
	}
	return &TierChoice{Tier: choice, EstimatedTokens: estimated, Reason: reason}
}

// findNamedTier 按名称或 token 数匹配档位（忽略空格与大小写）。
func findNamedTier(tiers []Tier, name string) (Tier, bool) {
	wanted := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), " ", ""))
	if wanted == "" {
		return Tier{}, false
	}
	wantedCount := parseTierTokenCount(wanted)
	for _, t := range tiers {
		if strings.ToUpper(strings.ReplaceAll(t.Name, " ", "")) == wanted {
			return t, true
		}
		if wantedCount > 0 && t.TokenCount == wantedCount {
			return t, true
		}
	}
	return Tier{}, false
}

// ApplyTier 把选中的档位写进 chat payload（对应 IDE 写法）。
func ApplyTier(payload map[string]any, tier Tier) {
	if payload == nil || tier.TokenCount <= 0 {
		return
	}
	params, _ := payload["parameters"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	params["context_length"] = tier.TokenCount
	payload["parameters"] = params

	chatCtx, _ := payload["chat_context"].(map[string]any)
	if chatCtx == nil {
		chatCtx = map[string]any{}
	}
	extra, _ := chatCtx["extra"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
	}
	override, _ := extra["ideModelConfigOverride"].(map[string]any)
	if override == nil {
		override = map[string]any{}
	}
	override["max_input_tokens"] = tier.TokenCount
	extra["ideModelConfigOverride"] = override
	chatCtx["extra"] = extra
	payload["chat_context"] = chatCtx

	if cfg, ok := payload["model_config"].(map[string]any); ok {
		cfg["max_input_tokens"] = tier.TokenCount
		payload["model_config"] = cfg
	}
}

// tierPreference 读取档位策略：显式覆盖优先，否则读环境变量。
func tierPreference(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return os.Getenv(ContextTierEnv)
}

// truncateRunes 按字符截断（对应 JS 的 slice(0, n)）。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
