package workbuddy

import (
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// realm_test.go 区域识别与端点拼接（G1 的回归测试）。
//
// 重点：base_url 里可能已经被用户/旧预设写入了路径成分，拼接必须幂等。

func TestRealmOfByOAuthProvider(t *testing.T) {
	cases := []struct {
		name string
		p    *model.Provider
		want Realm
		ok   bool
	}{
		{"intl 适配器", &model.Provider{OAuthProvider: ProviderIntl}, RealmIntl, true},
		{"cn 适配器", &model.Provider{OAuthProvider: ProviderCN}, RealmCN, true},
		{"无关供应商", &model.Provider{OAuthProvider: "kimi"}, "", false},
		{"nil 渠道", nil, "", false},
		{"intl 域名兜底", &model.Provider{BaseURL: "https://www.workbuddy.ai"}, RealmIntl, true},
		{"intl 手填路径", &model.Provider{BaseURL: "https://www.workbuddy.ai/v2"}, RealmIntl, true},
		{"cn chat 域名", &model.Provider{BaseURL: "https://copilot.tencent.com/v2"}, RealmCN, true},
		{"cn billing 域名", &model.Provider{BaseURL: "https://www.codebuddy.cn"}, RealmCN, true},
		{"cn workbuddy.cn", &model.Provider{BaseURL: "https://workbuddy.cn/api"}, RealmCN, true},
		{"空 base_url", &model.Provider{}, "", false},
		{"其它域名", &model.Provider{BaseURL: "https://api.deepseek.com"}, "", false},
		{"大小写与端口", &model.Provider{BaseURL: "HTTPS://WWW.WorkBuddy.AI:443/x"}, RealmIntl, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RealmOf(tc.p)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("RealmOf() = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
			if IsWorkBuddy(tc.p) != tc.ok {
				t.Fatalf("IsWorkBuddy() = %v, want %v", !tc.ok, tc.ok)
			}
		})
	}
}

func TestEndpointUsesChatPath(t *testing.T) {
	cases := []struct {
		name    string
		p       *model.Provider
		wantURL string
	}{
		{
			name:    "intl 默认",
			p:       &model.Provider{OAuthProvider: ProviderIntl},
			wantURL: "https://www.workbuddy.ai/v2/chat/completions",
		},
		{
			name:    "cn 默认",
			p:       &model.Provider{OAuthProvider: ProviderCN},
			wantURL: "https://copilot.tencent.com/v2/chat/completions",
		},
		{
			// 旧预设的历史值：不归一化就会拼成 /v2/v2/chat/completions
			name:    "cn 旧预设 base_url 带 /v2",
			p:       &model.Provider{OAuthProvider: ProviderCN, BaseURL: "https://copilot.tencent.com/v2"},
			wantURL: "https://copilot.tencent.com/v2/chat/completions",
		},
		{
			name:    "base_url 已含完整 chat 路径",
			p:       &model.Provider{OAuthProvider: ProviderCN, BaseURL: "https://copilot.tencent.com/v2/chat/completions"},
			wantURL: "https://copilot.tencent.com/v2/chat/completions",
		},
		{
			name:    "base_url 带 /v1/chat/completions",
			p:       &model.Provider{OAuthProvider: ProviderIntl, BaseURL: "https://www.workbuddy.ai/v1/chat/completions"},
			wantURL: "https://www.workbuddy.ai/v2/chat/completions",
		},
		{
			name:    "尾斜杠",
			p:       &model.Provider{OAuthProvider: ProviderIntl, BaseURL: "https://www.workbuddy.ai/"},
			wantURL: "https://www.workbuddy.ai/v2/chat/completions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Endpoint(tc.p)
			if err != nil {
				t.Fatalf("Endpoint() error = %v", err)
			}
			if got != tc.wantURL {
				t.Fatalf("Endpoint() = %q, want %q", got, tc.wantURL)
			}
			// 幂等：把结果当作 base_url 再算一次必须得到同一个 URL
			tc.p.BaseURL = got
			again, err := Endpoint(tc.p)
			if err != nil || again != got {
				t.Fatalf("Endpoint() 不幂等: %q (err=%v)", again, err)
			}
		})
	}
}

func TestEndpointRejectsForeignProvider(t *testing.T) {
	if _, err := Endpoint(&model.Provider{OAuthProvider: "kimi"}); err == nil {
		t.Fatal("非 workbuddy 渠道应当报错")
	}
	if _, err := Endpoint(nil); err == nil {
		t.Fatal("nil 渠道应当报错")
	}
}

func TestAuxiliaryEndpoints(t *testing.T) {
	if got := CreditsEndpoint(RealmCN); got != "https://www.codebuddy.cn/v2/billing/meter/get-user-resource" {
		t.Fatalf("CreditsEndpoint(cn) = %q", got)
	}
	if got := CreditsEndpoint(RealmIntl); got != "https://www.workbuddy.ai/v2/billing/meter/get-user-resource" {
		t.Fatalf("CreditsEndpoint(intl) = %q", got)
	}
	if got := CheckinEndpoint(RealmCN); got != "https://www.codebuddy.cn/v2/billing/meter/daily-checkin" {
		t.Fatalf("CheckinEndpoint(cn) = %q", got)
	}
	p := &model.Provider{OAuthProvider: ProviderIntl}
	if got, err := ModelsEndpoint(p); err != nil || got != "https://www.workbuddy.ai/v2/enterprises/personal/models" {
		t.Fatalf("ModelsEndpoint() = %q, %v", got, err)
	}
	if got, err := RemoteConfigEndpoint(p); err != nil || got != "https://www.workbuddy.ai/v3/config" {
		t.Fatalf("RemoteConfigEndpoint() = %q, %v", got, err)
	}
	if _, err := WebBase(RealmCN); err == nil {
		t.Fatal("国内版无网页 ACP 通道，应当报错")
	}
	if got, err := WebBase(RealmIntl); err != nil || got != "https://www.workbuddy.ai" {
		t.Fatalf("WebBase(intl) = %q, %v", got, err)
	}
}

func TestNormalizeBaseRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "copilot.tencent.com", "not a url"} {
		if got := normalizeBase(raw); got != "" {
			t.Fatalf("normalizeBase(%q) = %q, want empty", raw, got)
		}
	}
}
