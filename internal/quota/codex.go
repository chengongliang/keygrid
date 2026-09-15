// Package quota OpenAI Codex（ChatGPT 订阅）渠道额度同步：
//
//   - 主动探测：GET {backend-api}/wham/usage（Bearer access_token + ChatGPT-Account-Id），
//     一次拿到周限额/短窗口用量、重置时间、重置次数、credits 与模型级附加限额；
//   - 被动观察：relay 转发上游响应自带的 x-codex-* 头（cli-proxy-api quota signals
//     同款思路），零成本补齐活跃渠道的快照。
//
// 数据落 quota_snapshots（quota_sync 周期任务 + POST /api/providers/{id}/quota/refresh
// 手动刷新写入；GET /api/quota 读快照展示）。
//
// 端点与响应结构考据（官方 codex-rs backend-client）：
//
//	GET https://chatgpt.com/backend-api/wham/usage
//	Authorization: Bearer <OAuth access_token>
//	ChatGPT-Account-Id: <chatgpt_account_id>
//	→ { plan_type, rate_limit: { allowed, limit_reached,
//	    primary_window:   { used_percent, limit_window_seconds, reset_after_seconds, reset_at },
//	    secondary_window: { ... } },
//	    credits, additional_rate_limits[], rate_limit_reset_credits: { available_count } }
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

const (
	// codexUsagePath ChatGPT backend 的用量查询路径（codex-rs PathStyle::ChatGptApi）。
	codexUsagePath = "/wham/usage"
	// codexUsageUserAgent 对齐官方 CLI（codex-rs headers()）。
	codexUsageUserAgent = "codex-cli"
	codexAccountHeader  = "ChatGPT-Account-Id"
	// codexUsageTimeout 单次用量查询超时。
	codexUsageTimeout = 15 * time.Second
)

// SupportsQuota 渠道是否支持额度查询（当前仅 openai oauth；其他订阅平台如提供
// 类似用量端点可在此扩展）。
func SupportsQuota(p *model.Provider) bool {
	return p != nil && p.Kind == "oauth" && p.OAuthProvider == "openai"
}

// CodexUsageURL 从渠道 base_url 派生 /wham/usage 端点。
// base_url 约定为完整 Responses endpoint（https://chatgpt.com/backend-api/codex/responses），
// 截掉尾段得 backend-api base 再拼路径；非标准形态回退同 origin 的官方路径。
func CodexUsageURL(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{"/codex/responses", "/responses"} {
		if strings.HasSuffix(b, suffix) {
			return strings.TrimSuffix(b, suffix) + codexUsagePath
		}
	}
	if u, err := url.Parse(b); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/backend-api" + codexUsagePath
	}
	return b + codexUsagePath
}

// FetchCodexUsage 执行一次 /wham/usage 查询并解析。
// accountID 为凭据 extra 里的 chatgpt_account_id（多账户/团队场景必需）。
func FetchCodexUsage(ctx context.Context, client *http.Client, usageURL, accessToken, accountID string) (*model.QuotaData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexUsageUserAgent)
	if accountID != "" {
		req.Header.Set(codexAccountHeader, accountID)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("usage query unauthorized (HTTP %d): token expired or revoked, re-auth required", resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("usage query failed (HTTP %d): %s", resp.StatusCode, truncErrBody(body))
	}
	return ParseCodexUsage(body)
}

// ParseCodexUsage 解析 /wham/usage 响应。字段兼容：
//   - 窗口大小两种表示：limit_window_seconds（主动探测形状）/ window_minutes（websocket 形状）
//   - 驼峰变体（usedPercent / resetAfterSeconds / resetAt）
//   - additional_rate_limits 两形状：HTTP 为数组、websocket 为对象（cli-proxy-api 同款兼容）
func ParseCodexUsage(body []byte) (*model.QuotaData, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("codex usage response: %w", err)
	}
	data := &model.QuotaData{}
	data.PlanType = getStr(raw, "plan_type", "planType")
	// 键兼容：主动探测为 rate_limit；websocket 事件形状为 rate_limits（复数）
	if rl := getObj(raw, "rate_limit", "rate_limits"); rl != nil {
		data.RateLimit = parseRateLimit(rl)
	}
	if c := getObj(raw, "credits"); c != nil {
		data.Credits = parseCredits(c)
	}
	if rc := getObj(raw, "rate_limit_reset_credits", "rateLimitResetCredits"); rc != nil {
		if n := getNum(rc, "available_count", "availableCount"); n != nil {
			v := int(*n)
			data.ResetCreditsAvailable = &v
		}
	}
	data.Additional = parseAdditional(raw["additional_rate_limits"])
	return data, nil
}

// parseRateLimit 单条限额：allowed/limit_reached + primary/secondary 窗口。
// 窗口键兼容 *_window（主动探测）与裸 primary/secondary（websocket 事件）两形状。
func parseRateLimit(m map[string]any) *model.QuotaRateLimit {
	rl := &model.QuotaRateLimit{
		Allowed:      getBoolDef(m, true, "allowed"),
		LimitReached: getBoolDef(m, false, "limit_reached", "limitReached"),
	}
	rl.Primary = parseWindowAny(m, "primary_window", "primary")
	rl.Secondary = parseWindowAny(m, "secondary_window", "secondary")
	return rl
}

// parseWindowAny 依次尝试多个键，取到对象即解析窗口。
func parseWindowAny(m map[string]any, keys ...string) *model.QuotaWindow {
	for _, k := range keys {
		if w := getObj(m, k); w != nil {
			return parseWindow(w)
		}
	}
	return nil
}

// parseWindow 单窗口字段解析（snake_case 优先，驼峰兜底）。
func parseWindow(m map[string]any) *model.QuotaWindow {
	w := &model.QuotaWindow{}
	if v := getNum(m, "used_percent", "usedPercent"); v != nil {
		w.UsedPercent = *v
	}
	if v := getNum(m, "limit_window_seconds", "limitWindowSeconds"); v != nil {
		w.WindowSeconds = int64(*v)
	}
	// websocket 形状：window_minutes（分钟）
	if v := getNum(m, "window_minutes", "windowMinutes"); v != nil && w.WindowSeconds == 0 {
		w.WindowSeconds = int64(*v) * 60
	}
	if v := getNum(m, "reset_after_seconds", "resetAfterSeconds"); v != nil {
		w.ResetAfterSec = int64(*v)
	}
	if v := getNum(m, "reset_at", "resetAt"); v != nil {
		w.ResetAt = int64(*v)
	}
	// reset_at 缺失时从 reset_after 推算（绝对时间展示统一）
	if w.ResetAt == 0 && w.ResetAfterSec > 0 {
		w.ResetAt = time.Now().Unix() + w.ResetAfterSec
	}
	w.WindowLabel = WindowLabel(w.WindowSeconds)
	return w
}

// parseCredits credits 状态（缺失字段取零值）。
func parseCredits(m map[string]any) *model.QuotaCredits {
	return &model.QuotaCredits{
		HasCredits: getBoolDef(m, false, "has_credits", "hasCredits"),
		Unlimited:  getBoolDef(m, false, "unlimited"),
		Balance:    getStr(m, "balance"),
	}
}

// parseAdditional additional_rate_limits 两形状归一化：
//   - 数组：[{limit_name, rate_limit: {...}}, ...]
//   - 对象：{"GPT-5.3-Codex-Spark": {allowed, primary, secondary}, ...}
func parseAdditional(v any) []model.QuotaAdditional {
	var out []model.QuotaAdditional
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			add := model.QuotaAdditional{LimitName: getStr(m, "limit_name", "limitName")}
			if rl := getObj(m, "rate_limit"); rl != nil {
				add.RateLimit = parseRateLimit(rl)
			}
			out = append(out, add)
		}
	case map[string]any:
		for name, raw := range t {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, model.QuotaAdditional{LimitName: name, RateLimit: parseRateLimit(m)})
		}
	}
	return out
}

// WindowLabel 窗口大小 → 可读标签（官方 codex CLI get_limits_duration 同款近似匹配，
// 相对误差 ≤20% 即视为该档位；不假设 primary/secondary 的固定含义）。
func WindowLabel(windowSeconds int64) string {
	if windowSeconds <= 0 {
		return ""
	}
	m := windowSeconds / 60
	labels := []struct {
		min  int64
		name string
	}{
		{300, "5h"},       // 5 小时短窗口
		{1440, "daily"},   // 日窗口
		{10080, "weekly"}, // 7*24*60 周限额
		{43200, "monthly"},
		{525600, "yearly"},
	}
	for _, l := range labels {
		diff := m - l.min
		if diff < 0 {
			diff = -diff
		}
		if diff*5 <= l.min {
			return l.name
		}
	}
	return ""
}

// ---- 被动观察：转发响应的 x-codex-* 头（cli-proxy-api quota signals 同款命名）----
//
//	 X-Codex-Plan-Type / X-Codex-Allowed / X-Codex-Limit-Reached
//	 X-Codex-{Primary,Secondary}-{Used-Percent,Window-Minutes,Reset-After-Seconds,Reset-At}
//
// 响应头数据只有 rate_limit 部分（无 credits / reset credits / additional），
// 合并时保留主动探测写入的其余字段（见 Syncer.mergeObservation）。

// HasQuotaHeaders 响应是否携带额度水印（relay 被动观察的快速判定用）。
func HasQuotaHeaders(h http.Header) bool {
	return h.Get("X-Codex-Primary-Used-Percent") != "" || h.Get("X-Codex-Secondary-Used-Percent") != ""
}

// ParseCodexHeaders 解析 x-codex-* 响应头；无额度头时返回 nil。
func ParseCodexHeaders(h http.Header) *model.QuotaData {
	if !HasQuotaHeaders(h) {
		return nil
	}
	data := &model.QuotaData{}
	if v := h.Get("X-Codex-Plan-Type"); v != "" {
		data.PlanType = strings.ToLower(v)
	}
	rl := &model.QuotaRateLimit{
		Allowed:      parseOnOff(h.Get("X-Codex-Allowed"), true),
		LimitReached: parseOnOff(h.Get("X-Codex-Limit-Reached"), false),
	}
	rl.Primary = parseWindowHeaders(h, "Primary")
	rl.Secondary = parseWindowHeaders(h, "Secondary")
	data.RateLimit = rl
	return data
}

// parseWindowHeaders 单窗口的四个头 → QuotaWindow（Used-Percent 缺失视为无该窗口）。
func parseWindowHeaders(h http.Header, which string) *model.QuotaWindow {
	up := h.Get("X-Codex-" + which + "-Used-Percent")
	if up == "" {
		return nil
	}
	pct, err := strconv.ParseFloat(strings.TrimSpace(up), 64)
	if err != nil {
		return nil
	}
	w := &model.QuotaWindow{UsedPercent: pct}
	if v := h.Get("X-Codex-" + which + "-Window-Minutes"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			w.WindowSeconds = n * 60
		}
	}
	if v := h.Get("X-Codex-" + which + "-Reset-After-Seconds"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			w.ResetAfterSec = n
		}
	}
	if v := h.Get("X-Codex-" + which + "-Reset-At"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			w.ResetAt = n
		}
	}
	if w.ResetAt == 0 && w.ResetAfterSec > 0 {
		w.ResetAt = time.Now().Unix() + w.ResetAfterSec
	}
	w.WindowLabel = WindowLabel(w.WindowSeconds)
	return w
}

// parseOnOff 头值布尔解析（true/1 = 开）。
func parseOnOff(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return def
}

// ---- 松散 JSON 取值 helpers（map[string]any 上多键兼容）----

func getStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func getBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		switch v := m[k].(type) {
		case bool:
			return v
		case string:
			if v == "true" {
				return true
			}
			if v == "false" {
				return false
			}
		}
	}
	return false
}

// getBoolDef 同 getBool，但支持默认值（键全部缺失时返回 def）。
func getBoolDef(m map[string]any, def bool, keys ...string) bool {
	for _, k := range keys {
		if _, exists := m[k]; exists {
			return getBool(m, k)
		}
	}
	return def
}

func getNum(m map[string]any, keys ...string) *float64 {
	for _, k := range keys {
		if n, ok := m[k].(float64); ok {
			return &n
		}
	}
	return nil
}

func getObj(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if o, ok := m[k].(map[string]any); ok {
			return o
		}
	}
	return nil
}

// truncErrBody 错误响应体截断（进 error message，不落敏感内容）。
func truncErrBody(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max]
	}
	return s
}
