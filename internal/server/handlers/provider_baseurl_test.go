package handlers

import (
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/oauth"
	// 触发 oauth provider 注册（生产环境由 main.go 空导入，测试同样需要）
	_ "github.com/chengongliang/keygrid/internal/oauth/providers"
)

// oauthDefaultBaseURL 必须以 oauth/presets.go 的 ProviderPresets 为单一事实来源：
// 历史遗留的 openai = https://chatgpt.com/backend-api 曾因「已占位不覆盖」被钉死，
// 新建 Codex 渠道的 base_url 因此缺 /codex/responses 尾段，请求被 POST 到非
// Responses 地址（客户端表现为 response.completed 前流中断）。
func TestOAuthDefaultBaseURLFollowsPresets(t *testing.T) {
	keys := oauth.Keys()
	if len(keys) == 0 {
		t.Fatal("oauth providers must be registered")
	}
	presetKeys := map[string]bool{}
	for _, p := range oauth.ProviderPresets() {
		if p.Kind != "oauth" {
			continue
		}
		presetKeys[p.Key] = true
		if got := oauthDefaultBaseURL[p.Key]; got != p.BaseURL {
			t.Errorf("oauthDefaultBaseURL[%s] = %q, want preset %q", p.Key, got, p.BaseURL)
		}
	}
	// 未在 presets 中登记的 oauth provider（anthropic）保留显式兜底
	for _, k := range keys {
		if !presetKeys[k] && oauthDefaultBaseURL[k] == "" {
			t.Errorf("oauth provider %s 缺默认 base_url（presets 与显式兜底都没有）", k)
		}
	}
	if got := oauthDefaultBaseURL["openai"]; !strings.HasSuffix(got, "/codex/responses") {
		t.Errorf("openai 默认 base_url 必须是完整 Responses endpoint，got %q", got)
	}
}

// normalizeOAuthBaseURL：Codex 渠道写时归一化；其余渠道/类型不受影响。
func TestNormalizeOAuthBaseURL(t *testing.T) {
	cases := []struct{ kind, provider, in, want string }{
		{"oauth", "openai", "https://chatgpt.com/backend-api", "https://chatgpt.com/backend-api/codex/responses"},
		{"oauth", "openai", "https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/backend-api/codex/responses"},
		{"oauth", "openai", "", ""},
		{"oauth", "kimi", "https://api.kimi.com/coding", "https://api.kimi.com/coding"},
		{"api_key", "openai", "https://chatgpt.com/backend-api", "https://chatgpt.com/backend-api"},
		{"api_key", "", "https://api.deepseek.com", "https://api.deepseek.com"},
	}
	for _, c := range cases {
		if got := normalizeOAuthBaseURL(c.kind, c.provider, c.in); got != c.want {
			t.Errorf("normalizeOAuthBaseURL(%q,%q,%q) = %q, want %q", c.kind, c.provider, c.in, got, c.want)
		}
	}
}
