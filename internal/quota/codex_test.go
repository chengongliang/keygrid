package quota

import (
	"net/http"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

// codex_test.go /wham/usage 解析、URL 派生、窗口标签、x-codex-* 被动观察头解析。
// 响应样本字段对齐官方 codex-rs backend-client 的 OpenAPI 模型与 CLIProxyAPI 抓包样本。

// 完整 /wham/usage 响应（主动探测形状：primary_window/secondary_window + snake_case）。
const sampleWhamUsage = `{
  "plan_type": "pro",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {
      "used_percent": 44,
      "limit_window_seconds": 604800,
      "reset_after_seconds": 378000,
      "reset_at": 1786677299
    },
    "secondary_window": {
      "used_percent": 55,
      "limit_window_seconds": 18000,
      "reset_after_seconds": 1500,
      "reset_at": 1786304000
    }
  },
  "credits": { "has_credits": false, "unlimited": false, "balance": "0" },
  "additional_rate_limits": [
    {
      "limit_name": "GPT-5.3-Codex-Spark",
      "metered_feature": "codex",
      "rate_limit": {
        "allowed": true,
        "limit_reached": false,
        "primary_window": { "used_percent": 3, "limit_window_seconds": 18000, "reset_after_seconds": 10148, "reset_at": 1787231961 },
        "secondary_window": { "used_percent": 63, "limit_window_seconds": 604800, "reset_after_seconds": 75420, "reset_at": 1787290791 }
      }
    }
  ],
  "rate_limit_reached_type": null,
  "rate_limit_reset_credits": { "available_count": 2 }
}`

func TestParseCodexUsage(t *testing.T) {
	d, err := ParseCodexUsage([]byte(sampleWhamUsage))
	if err != nil {
		t.Fatalf("ParseCodexUsage: %v", err)
	}
	if d.PlanType != "pro" {
		t.Errorf("plan_type = %q, want pro", d.PlanType)
	}
	rl := d.RateLimit
	if rl == nil || !rl.Allowed || rl.LimitReached {
		t.Errorf("rate_limit allowed/limit_reached = %+v", rl)
	}
	if rl.Primary == nil || rl.Primary.UsedPercent != 44 || rl.Primary.WindowSeconds != 604800 {
		t.Errorf("primary_window = %+v", rl.Primary)
	}
	if rl.Primary.WindowLabel != "weekly" {
		t.Errorf("primary label = %q, want weekly", rl.Primary.WindowLabel)
	}
	if rl.Primary.ResetAt != 1786677299 {
		t.Errorf("primary reset_at = %d", rl.Primary.ResetAt)
	}
	if rl.Secondary == nil || rl.Secondary.UsedPercent != 55 || rl.Secondary.WindowLabel != "5h" {
		t.Errorf("secondary_window = %+v", rl.Secondary)
	}
	if d.Credits == nil || d.Credits.Balance != "0" {
		t.Errorf("credits = %+v", d.Credits)
	}
	if d.ResetCreditsAvailable == nil || *d.ResetCreditsAvailable != 2 {
		t.Errorf("reset_credits_available = %v, want 2", d.ResetCreditsAvailable)
	}
	if len(d.Additional) != 1 || d.Additional[0].LimitName != "GPT-5.3-Codex-Spark" {
		t.Fatalf("additional = %+v", d.Additional)
	}
	if arl := d.Additional[0].RateLimit; arl == nil || arl.Primary == nil || arl.Primary.UsedPercent != 3 {
		t.Errorf("additional rate_limit = %+v", arl)
	}
}

// websocket 事件形状（CLIProxyAPI codex_quota_test.go 抓包样本）：裸 primary/secondary
// + window_minutes + additional_rate_limits 为对象。
const sampleWsEvent = `{
  "type": "codex.rate_limits",
  "plan_type": "pro",
  "metered_limit_name": "codex_bengalfox",
  "rate_limits": {
    "allowed": true,
    "limit_reached": false,
    "primary": { "used_percent": 48, "window_minutes": 10080, "reset_after_seconds": 523210, "reset_at": 1786677299 },
    "secondary": null
  },
  "additional_rate_limits": {
    "GPT-5.3-Codex-Spark": {
      "allowed": true, "limit_reached": false,
      "primary": { "used_percent": 3, "window_minutes": 300, "reset_after_seconds": 10148, "reset_at": 1787231961 },
      "secondary": { "used_percent": 63, "window_minutes": 10080, "reset_after_seconds": 75420, "reset_at": 1787290791 }
    }
  },
  "credits": { "has_credits": false, "unlimited": false, "balance": "0" }
}`

func TestParseCodexUsageWebsocketShape(t *testing.T) {
	d, err := ParseCodexUsage([]byte(sampleWsEvent))
	if err != nil {
		t.Fatalf("ParseCodexUsage: %v", err)
	}
	if d.PlanType != "pro" {
		t.Errorf("plan_type = %q", d.PlanType)
	}
	rl := d.RateLimit
	if rl == nil || rl.Primary == nil {
		t.Fatalf("rate_limit = %+v", rl)
	}
	if rl.Primary.UsedPercent != 48 || rl.Primary.WindowSeconds != 10080*60 || rl.Primary.WindowLabel != "weekly" {
		t.Errorf("primary = %+v", rl.Primary)
	}
	if rl.Secondary != nil {
		t.Errorf("secondary should be nil (null), got %+v", rl.Secondary)
	}
	if len(d.Additional) != 1 || d.Additional[0].LimitName != "GPT-5.3-Codex-Spark" {
		t.Fatalf("additional = %+v", d.Additional)
	}
	// 对象形状的 additional 元素里 allowed 缺省视为 true
	if arl := d.Additional[0].RateLimit; arl == nil || !arl.Allowed {
		t.Errorf("additional allowed = %+v", arl)
	}
}

// reset_at 缺失时由 reset_after_seconds 推算绝对时间。
func TestParseCodexUsageResetAtFallback(t *testing.T) {
	d, err := ParseCodexUsage([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`))
	if err != nil {
		t.Fatalf("ParseCodexUsage: %v", err)
	}
	w := d.RateLimit.Primary
	if w.ResetAt == 0 {
		t.Fatalf("reset_at should be derived from reset_after_seconds")
	}
	now := time.Now().Unix()
	if w.ResetAt < now+3590 || w.ResetAt > now+3610 {
		t.Errorf("reset_at = %d, want ~now+3600 (%d)", w.ResetAt, now)
	}
}

func TestCodexUsageURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/backend-api/wham/usage"},
		{"https://chatgpt.com/backend-api/codex/responses/", "https://chatgpt.com/backend-api/wham/usage"},
		{"https://chatgpt.com/backend-api/responses", "https://chatgpt.com/backend-api/wham/usage"},
		{"https://chatgpt.com/backend-api/codex", "https://chatgpt.com/backend-api/wham/usage"},
		{"http://mock-openai.internal:8080/v1/codex/responses", "http://mock-openai.internal:8080/v1/wham/usage"},
		{"http://127.0.0.1:9999/api", "http://127.0.0.1:9999/backend-api/wham/usage"},
	}
	for _, c := range cases {
		if got := CodexUsageURL(c.in); got != c.want {
			t.Errorf("CodexUsageURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWindowLabel(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, ""},
		{18000, "5h"},      // 5 小时
		{14400, "5h"},      // 4h（±20% 内）
		{86400, "daily"},   // 1 天
		{604800, "weekly"}, // 7 天
		{504000, "weekly"}, // ~5.8 天（±20% 内）
		{43200, ""},        // 12h 不落在任何档位
	}
	for _, c := range cases {
		if got := WindowLabel(c.secs); got != c.want {
			t.Errorf("WindowLabel(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestParseCodexHeaders(t *testing.T) {
	// 无额度头 → nil
	if d := ParseCodexHeaders(http.Header{}); d != nil {
		t.Fatalf("empty headers should return nil, got %+v", d)
	}
	h := http.Header{}
	h.Set("X-Codex-Plan-Type", "pro")
	h.Set("X-Codex-Allowed", "true")
	h.Set("X-Codex-Limit-Reached", "false")
	h.Set("X-Codex-Primary-Used-Percent", "48")
	h.Set("X-Codex-Primary-Window-Minutes", "10080")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "523210")
	h.Set("X-Codex-Primary-Reset-At", "1786677299")
	h.Set("X-Codex-Secondary-Used-Percent", "7")
	h.Set("X-Codex-Secondary-Window-Minutes", "300")
	h.Set("X-Codex-Secondary-Reset-After-Seconds", "10148")

	d := ParseCodexHeaders(h)
	if d == nil {
		t.Fatal("ParseCodexHeaders nil")
	}
	if d.PlanType != "pro" {
		t.Errorf("plan_type = %q", d.PlanType)
	}
	rl := d.RateLimit
	if !rl.Allowed || rl.LimitReached {
		t.Errorf("allowed/limit_reached = %+v", rl)
	}
	if rl.Primary == nil || rl.Primary.UsedPercent != 48 || rl.Primary.WindowSeconds != 10080*60 {
		t.Errorf("primary = %+v", rl.Primary)
	}
	if rl.Primary.WindowLabel != "weekly" || rl.Primary.ResetAt != 1786677299 {
		t.Errorf("primary label/reset_at = %+v", rl.Primary)
	}
	if rl.Secondary == nil || rl.Secondary.UsedPercent != 7 || rl.Secondary.WindowLabel != "5h" {
		t.Errorf("secondary = %+v", rl.Secondary)
	}
}

// 429 限流响应头：limit_reached=true（被动观察应记录限额触达水印）。
func TestParseCodexHeadersLimitReached(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "100")
	h.Set("X-Codex-Allowed", "false")
	h.Set("X-Codex-Limit-Reached", "true")
	d := ParseCodexHeaders(h)
	if d == nil || d.RateLimit == nil || !d.RateLimit.LimitReached || d.RateLimit.Allowed {
		t.Fatalf("rate_limit = %+v", d)
	}
}

func TestSupportsQuota(t *testing.T) {
	if !SupportsQuota(&model.Provider{Kind: "oauth", OAuthProvider: "openai"}) {
		t.Error("openai oauth channel should be supported")
	}
	for _, p := range []*model.Provider{
		nil,
		{Kind: "api_key"},
		{Kind: "oauth", OAuthProvider: "kimi"},
		{Kind: "oauth", OAuthProvider: "anthropic"},
	} {
		if SupportsQuota(p) {
			t.Errorf("should not be supported: %+v", p)
		}
	}
}
