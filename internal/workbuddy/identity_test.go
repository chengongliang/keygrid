package workbuddy

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// identHex32 32 位小写 hex 的正则（请求/会话/指纹 id 的统一形状）。
var identHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// identFixedNow 测试注入的固定时间，保证 id 生成不依赖真实时钟。
func identFixedNow() time.Time { return time.Unix(1700000000, 123456789) }

// identProvider 构造各区域的渠道。
func identProvider(realm Realm) *model.Provider {
	if realm == RealmCN {
		return &model.Provider{OAuthProvider: ProviderCN, BaseURL: "https://copilot.tencent.com"}
	}
	return &model.Provider{OAuthProvider: ProviderIntl, BaseURL: "https://www.workbuddy.ai"}
}

// identToken 构造带 uid 的凭据。
func identToken(uid string) *oauth.TokenSet {
	return &oauth.TokenSet{AccessToken: "at-" + uid, Extra: map[string]string{"uid": uid}}
}

// identMatrixCase 区域 × 产品档 的期望身分头。
type identMatrixCase struct {
	name         string
	realm        Realm
	product      string
	domain       string
	ideName      string
	product2     string // X-Product
	version      string
	ua           string
	intentKey    string // X-Agent-Purpose 或 X-Agent-Intent
	intentValue  string
	extra        map[string]string
	conversation bool
}

func identMatrixCases() []identMatrixCase {
	return []identMatrixCase{
		{
			name: "intl/desktop", realm: RealmIntl, product: "", domain: "www.workbuddy.ai",
			ideName: "WorkBuddy", product2: "WorkBuddy", version: "5.8.4",
			ua:        "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0",
			intentKey: "X-Agent-Purpose", intentValue: "conversation",
			conversation: true,
		},
		{
			name: "cn/desktop", realm: RealmCN, product: DefaultProduct(), domain: "copilot.tencent.com",
			ideName: "WorkBuddy", product2: "WorkBuddy", version: "5.5.6",
			ua:        "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1",
			intentKey: "X-Agent-Purpose", intentValue: "conversation",
			conversation: true,
		},
		{
			name: "intl/vscode", realm: RealmIntl, product: "vscode", domain: "www.workbuddy.ai",
			ideName: "VSCode", product2: "SaaS", version: "1.119.0",
			ua:        "VSCode/1.119.0 CodeBuddy/4.12.39997873",
			intentKey: "X-Agent-Intent", intentValue: "craft",
			extra:        map[string]string{"X-Env-ID": "production", "X-Product-Version": "4.12.39997873"},
			conversation: true,
		},
		{
			name: "cn/vscode", realm: RealmCN, product: "vscode", domain: "www.workbuddy.cn",
			ideName: "VSCode", product2: "SaaS", version: "1.119.0",
			ua:        "VSCode/1.119.0 CodeBuddy/4.12.39997873",
			intentKey: "X-Agent-Intent", intentValue: "craft",
			extra:        map[string]string{"X-Env-ID": "production", "X-Product-Version": "4.12.39997873"},
			conversation: true,
		},
		{
			name: "intl/cli", realm: RealmIntl, product: "cli", domain: "www.workbuddy.ai",
			ideName: "CLI", product2: "SaaS", version: "2.168.2",
			ua:        "CLI/2.168.2 CodeBuddy/2.168.2",
			intentKey: "X-Agent-Intent", intentValue: "craft",
			conversation: true,
		},
		{
			name: "cn/cli", realm: RealmCN, product: "cli", domain: "copilot.tencent.com",
			ideName: "CLI", product2: "SaaS", version: "2.168.2",
			ua:        "CLI/2.168.2 CodeBuddy/2.168.2",
			intentKey: "X-Agent-Intent", intentValue: "craft",
			conversation: true,
		},
	}
}

func TestIdentHeadersMatrix(t *testing.T) {
	for _, tc := range identMatrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Headers(identProvider(tc.realm), identToken("uid-123"), HeaderOpt{
				Product: tc.product, Now: identFixedNow,
			})
			if err != nil {
				t.Fatalf("Headers 返回错误: %v", err)
			}

			// 固定部分。
			want := map[string]string{
				"Content-Type":        "application/json",
				"Accept":              "application/json",
				"X-Requested-With":    "XMLHttpRequest",
				"X-CodeBuddy-Request": "1",
				"Origin":              "https://" + tc.domain,
				"Referer":             "https://" + tc.domain + "/",
				"Accept-Language":     "zh-CN,zh;q=0.9",
				// 公共身分头。
				"X-Domain":      tc.domain,
				"X-IDE-Version": tc.version,
				"X-User-Id":     "uid-123",
				"User-Agent":    tc.ua,
				"X-IDE-Name":    tc.ideName,
				"X-IDE-Type":    tc.ideName,
				"X-Product":     tc.product2,
				tc.intentKey:    tc.intentValue,
			}
			for k, v := range want {
				if got := h.Get(k); got != v {
					t.Errorf("头 %s = %q, 期望 %q", k, got, v)
				}
			}
			for k, v := range tc.extra {
				if got := h.Get(k); got != v {
					t.Errorf("头 %s = %q, 期望 %q", k, got, v)
				}
			}
			// 非本档的 intent 头不应出现。
			if tc.product == "vscode" {
				if h.Get("X-Agent-Purpose") != "" {
					t.Errorf("vscode/cli 不应设置 X-Agent-Purpose")
				}
			} else if tc.intentKey == "X-Agent-Purpose" && h.Get("X-Agent-Intent") != "" {
				t.Errorf("desktop 不应设置 X-Agent-Intent")
			}
			// Authorization 永不设置。
			if h.Get("Authorization") != "" {
				t.Errorf("Headers 不应设置 Authorization")
			}
		})
	}
}

func TestIdentHeadersNotWorkBuddy(t *testing.T) {
	p := &model.Provider{OAuthProvider: "openai", BaseURL: "https://api.openai.com"}
	if _, err := Headers(p, identToken("u"), HeaderOpt{}); err != ErrNotWorkBuddy {
		t.Fatalf("非 workbuddy 渠道应返回 ErrNotWorkBuddy, 实得 %v", err)
	}
}

func TestIdentFingerprint(t *testing.T) {
	p := identProvider(RealmIntl)
	tok := identToken("uid-fp")

	// 默认不带指纹头。
	def, err := Headers(p, tok, HeaderOpt{Now: identFixedNow})
	if err != nil {
		t.Fatalf("Headers 错误: %v", err)
	}
	if def.Get("X-Machine-ID") != "" || def.Get("X-Session-ID") != "" {
		t.Fatalf("默认不应带指纹头: machine=%q session=%q", def.Get("X-Machine-ID"), def.Get("X-Session-ID"))
	}

	// 开启后为 32 位小写 hex，且两次调用稳定一致。
	h1, _ := Headers(p, tok, HeaderOpt{Fingerprint: true, Now: identFixedNow})
	h2, _ := Headers(p, tok, HeaderOpt{Fingerprint: true, Now: identFixedNow})
	for _, key := range []string{"X-Machine-ID", "X-Session-ID"} {
		v1, v2 := h1.Get(key), h2.Get(key)
		if !identHex32.MatchString(v1) {
			t.Errorf("%s = %q, 期望 32 位小写 hex", key, v1)
		}
		if v1 != v2 {
			t.Errorf("%s 两次调用不稳定: %q != %q", key, v1, v2)
		}
	}
	if h1.Get("X-Machine-ID") == h1.Get("X-Session-ID") {
		t.Errorf("machine 与 session 指纹不应相同")
	}
	// 与期望的 md5 派生值一致。
	wantMachine := md5Hex("keygrid:workbuddy:machine:uid-fp")
	if got := h1.Get("X-Machine-ID"); got != wantMachine {
		t.Errorf("X-Machine-ID = %q, 期望 %q", got, wantMachine)
	}
}

func TestIdentConversationHeaders(t *testing.T) {
	for _, product := range []string{"workbuddy", "vscode", "cli"} {
		t.Run(product, func(t *testing.T) {
			h, err := Headers(identProvider(RealmIntl), identToken("uid-conv"), HeaderOpt{
				Product: product, Now: identFixedNow,
			})
			if err != nil {
				t.Fatalf("Headers 错误: %v", err)
			}
			keys := []string{
				"X-Conversation-ID", "X-Conversation-Request-ID",
				"X-Conversation-Message-ID",
			}
			seen := map[string]string{}
			for _, k := range keys {
				v := strings.ReplaceAll(h.Get(k), "-", "")
				if !identHex32.MatchString(v) {
					t.Errorf("头 %s = %q, 期望 32 位小写 hex", k, v)
					continue
				}
				if prev, dup := seen[v]; dup {
					t.Errorf("头 %s 与 %s 生成了相同的 id: %q", k, prev, v)
				}
				seen[v] = k
			}
			if product == "vscode" {
				if h.Get("X-Request-ID") != h.Get("X-Conversation-Request-ID") || h.Get("X-Root-Request-ID") != "" {
					t.Fatal("VSCode 请求 id 关系错误")
				}
			} else if h.Get("X-Request-ID") != h.Get("X-Conversation-Message-ID") || h.Get("X-Root-Request-ID") != h.Get("X-Conversation-Request-ID") {
				t.Fatal("桌面端/CLI 请求 id 关系错误")
			}
		})
	}
}

func TestIdentConversationIDProvided(t *testing.T) {
	h, err := Headers(identProvider(RealmIntl), identToken("uid-conv"), HeaderOpt{
		Product: "vscode", ConversationID: "conv-fixed-1", Now: identFixedNow,
	})
	if err != nil {
		t.Fatalf("Headers 错误: %v", err)
	}
	if got := h.Get("X-Conversation-ID"); got != "conv-fixed-1" {
		t.Errorf("X-Conversation-ID = %q, 期望沿用 opt.ConversationID", got)
	}
}

func TestIdentDesktopConversation(t *testing.T) {
	h, err := Headers(identProvider(RealmIntl), identToken("uid-desk"), HeaderOpt{Now: identFixedNow})
	if err != nil {
		t.Fatalf("Headers 错误: %v", err)
	}
	if !identHex32.MatchString(h.Get("X-Request-ID")) {
		t.Errorf("desktop 应有 32 位 hex 的 X-Request-ID, 实得 %q", h.Get("X-Request-ID"))
	}
	if len(h.Get("X-Conversation-ID")) != 36 {
		t.Errorf("desktop 应有 UUID 会话 id, 实得 %q", h.Get("X-Conversation-ID"))
	}
}

func TestIdentConversationIDsAreUniqueAcrossRequests(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		headers, err := Headers(identProvider(RealmIntl), identToken("same-user"), HeaderOpt{Now: identFixedNow})
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
			identifier := strings.ReplaceAll(headers.Get(key), "-", "")
			if seen[identifier] {
				t.Fatal("同一时刻不同请求生成了重复 id")
			}
			if len(identifier) != 32 || identifier[12] != '7' || !strings.ContainsRune("89ab", rune(identifier[16])) {
				t.Fatalf("不是 UUIDv7: %q", identifier)
			}
			seen[identifier] = true
		}
	}
}

func TestIdentAccountDomainAndCLIPrivacy(t *testing.T) {
	t.Setenv("WB_CN_CHAT_UPSTREAM", "https://www.workbuddy.cn")
	token := identToken("uid-cn")
	token.Extra["domain"] = "copilot.tencent.com"
	for _, enterprise := range []string{"", "enterprise-test"} {
		token.Extra["enterprise_id"] = enterprise
		headers, err := Headers(identProvider(RealmCN), token, HeaderOpt{Product: ProductCLI})
		if err != nil {
			t.Fatal(err)
		}
		if headers.Get("X-Domain") != "copilot.tencent.com" {
			t.Fatal("账号 domain 不应跟随连接域名改变")
		}
		wantPrivate := "false"
		if enterprise != "" {
			wantPrivate = "true"
		}
		if headers.Get("X-Private-Data") != wantPrivate || headers.Get("X-Agent-Runtime") != "other" || headers.Get("X-Agent-Type") != "main" {
			t.Fatalf("CLI 账号身分错误: %v", headers)
		}
	}
}

func TestIdentUnknownProductFallsBackToDesktop(t *testing.T) {
	p := identProvider(RealmIntl)
	tok := identToken("uid-u")

	fallback, err := Headers(p, tok, HeaderOpt{Product: "bogus", Now: identFixedNow})
	if err != nil {
		t.Fatalf("Headers 错误: %v", err)
	}
	desktop, err := Headers(p, tok, HeaderOpt{Product: productDesktop, Now: identFixedNow})
	if err != nil {
		t.Fatalf("Headers 错误: %v", err)
	}
	for _, k := range []string{"X-IDE-Name", "X-IDE-Type", "X-Product", "X-IDE-Version", "User-Agent", "X-Agent-Purpose"} {
		if fallback.Get(k) != desktop.Get(k) {
			t.Errorf("未知 product 的 %s = %q, 期望回落到桌面档 %q", k, fallback.Get(k), desktop.Get(k))
		}
	}
	if len(fallback.Get("X-Conversation-ID")) != 36 {
		t.Errorf("未知 product 回落桌面档应带 UUID 会话头")
	}
}

func TestUIDFromExtra(t *testing.T) {
	tok := &oauth.TokenSet{AccessToken: "whatever", Extra: map[string]string{"uid": "  uid-from-extra  "}}
	if got := UIDFrom(tok); got != "uid-from-extra" {
		t.Errorf("UIDFrom = %q, 期望 uid-from-extra", got)
	}
}

func TestUIDFromJWTFallback(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"uid": "uid-from-jwt", "sub": "x"})
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	tok := &oauth.TokenSet{AccessToken: token, Extra: map[string]string{}}
	if got := UIDFrom(tok); got != "uid-from-jwt" {
		t.Errorf("UIDFrom = %q, 期望 uid-from-jwt", got)
	}

	// 带 padding 的 base64url 也应能解出。
	padded := "hdr." + base64.URLEncoding.EncodeToString(payload) + ".sig"
	if got := UIDFrom(&oauth.TokenSet{AccessToken: padded}); got != "uid-from-jwt" {
		t.Errorf("带 padding 的 JWT UIDFrom = %q, 期望 uid-from-jwt", got)
	}
}

func TestUIDFromEmpty(t *testing.T) {
	cases := []struct {
		name string
		tok  *oauth.TokenSet
	}{
		{"nil", nil},
		{"empty token", &oauth.TokenSet{}},
		{"no uid claim", &oauth.TokenSet{AccessToken: makeJWT(`{"sub":"y"}`)}},
		{"not a jwt", &oauth.TokenSet{AccessToken: "opaque-token"}},
		{"bad base64 payload", &oauth.TokenSet{AccessToken: "h.!!!notbase64!!!.s"}},
		{"uid not string", &oauth.TokenSet{AccessToken: makeJWT(`{"uid":123}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UIDFrom(tc.tok); got != "" {
				t.Errorf("UIDFrom = %q, 期望空串", got)
			}
		})
	}
}

// makeJWT 用给定的 JSON payload 拼一个 header.payload.sig 形态的假 token。
func makeJWT(payloadJSON string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(payloadJSON) + ".sig"
}

// md5Hex 期望值计算辅助（与被测代码同一算法，仅用于指纹稳定性断言）。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
