package workbuddy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// catalog_test.go 模型目录抓取（四段回落 / 防御式解析 / 过滤规则）。
//
// 全部走 httptest，不触网。上游事实是 realm.go 的常量，测试只覆盖本包的
// 解析与回落逻辑。

// catalogProvider 构造指向测试服务器的渠道（区域由 oauth_provider 权威判定）。
func catalogProvider(baseURL string, realm Realm) *model.Provider {
	key := ProviderIntl
	if realm == RealmCN {
		key = ProviderCN
	}
	return &model.Provider{OAuthProvider: key, BaseURL: baseURL}
}

// catalogEquals 断言两个字符串切片完全相等。
func catalogEquals(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func catalogContains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestFetchModelsConfigSuccess 覆盖 /v3/config 成功：字符串与对象两种条目形状，
// 并校验请求路径恰好是 /v3/config，且带桌面身分头。
func TestFetchModelsConfigSuccess(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "字符串条目",
			body: `{"code":0,"data":{"agents":[{"agentType":"cli","models":["hy4-preview-f","hy4-preview","auto","gpt-5.6-sol"]}]}}`,
			// auto 被过滤；hy4-preview 因存在 -f 档被替换掉。
			want: []string{"hy4-preview-f", "gpt-5.6-sol"},
		},
		{
			name: "对象条目",
			body: `{"code":0,"data":{"agents":[{"type":"cli","models":[{"id":"gpt-5.6-sol"},{"modelId":"glm-5.3"},{"name":"kimi-k3"}]}]}}`,
			want: []string{"gpt-5.6-sol", "glm-5.3", "kimi-k3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotHeader http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotHeader = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p := catalogProvider(srv.URL, RealmIntl)
			tok := &oauth.TokenSet{AccessToken: "tok-abc"}
			models, err := FetchModels(context.Background(), srv.Client(), p, tok)
			if err != nil {
				t.Fatalf("FetchModels() error = %v", err)
			}
			if !catalogEquals(models, tc.want) {
				t.Fatalf("models = %v, want %v", models, tc.want)
			}
			if gotPath != "/v3/config" {
				t.Fatalf("request path = %q, want /v3/config", gotPath)
			}
			if got := gotHeader.Get("X-Ide-Type"); got != "WorkBuddy" {
				t.Fatalf("X-IDE-Type = %q, want WorkBuddy", got)
			}
			if got := gotHeader.Get("X-Domain"); got != "www.workbuddy.ai" {
				t.Fatalf("X-Domain = %q, want www.workbuddy.ai", got)
			}
			if ua := gotHeader.Get("User-Agent"); !strings.Contains(ua, "WorkBuddy") {
				t.Fatalf("User-Agent = %q, want a desktop WorkBuddy UA", ua)
			}
			if got := gotHeader.Get("Authorization"); got != "Bearer tok-abc" {
				t.Fatalf("Authorization = %q, want Bearer tok-abc", got)
			}
		})
	}
}

// TestFetchModelsConfigFailsFallsBackToNarrow config 端点 400/12403 时回落窄端点。
func TestFetchModelsConfigFailsFallsBackToNarrow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/config", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":12403,"msg":"illegal invocation from an unapproved channel"}`))
	})
	var narrowHits int
	mux.HandleFunc("/v2/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		narrowHits++
		if r.URL.Path != "/v2/enterprises/personal/models" {
			t.Errorf("narrow path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3"},{"modelId":"kimi-k3-1"},"minimax-m3"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := catalogProvider(srv.URL, RealmIntl)
	models, err := FetchModels(context.Background(), srv.Client(), p, &oauth.TokenSet{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	want := []string{"glm-5.3", "kimi-k3-1", "minimax-m3"}
	if !catalogEquals(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	if narrowHits != 1 {
		t.Fatalf("narrow hits = %d, want 1", narrowHits)
	}
}

// TestFetchModelsSnapshotFallbackRealmAware 两个端点都失败 → 内嵌快照，且区分区域。
func TestFetchModelsSnapshotFallbackRealmAware(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer srv.Close()

	intl, err := FetchModels(context.Background(), srv.Client(), catalogProvider(srv.URL, RealmIntl), &oauth.TokenSet{})
	if err != nil {
		t.Fatalf("intl FetchModels() error = %v", err)
	}
	cn, err := FetchModels(context.Background(), srv.Client(), catalogProvider(srv.URL, RealmCN), &oauth.TokenSet{})
	if err != nil {
		t.Fatalf("cn FetchModels() error = %v", err)
	}
	if len(intl) == 0 || len(cn) == 0 {
		t.Fatalf("snapshot fallback empty: intl=%d cn=%d", len(intl), len(cn))
	}
	if catalogEquals(intl, cn) {
		t.Fatalf("snapshots for intl and cn must differ")
	}
	if !catalogContains(intl, "hy4-preview-f") || catalogContains(intl, "hy4-preview") {
		t.Fatalf("intl snapshot -f rule failed: %v", intl)
	}
	if catalogContains(cn, "default-1.1") || catalogContains(cn, "default-1.2") || catalogContains(cn, "hy3-x") {
		t.Fatalf("cn snapshot must drop default-*/ -x variants: %v", cn)
	}
	if !catalogContains(cn, "hy4-preview-f") || catalogContains(cn, "hy4-preview") {
		t.Fatalf("cn snapshot -f rule failed: %v", cn)
	}

	// SnapshotModels 本身也区分区域。
	if catalogEquals(SnapshotModels(RealmIntl), SnapshotModels(RealmCN)) {
		t.Fatalf("SnapshotModels must be realm-aware")
	}
	if SnapshotModels(Realm("unknown")) != nil {
		t.Fatalf("unknown realm should return nil snapshot")
	}
}

// TestFilterModelIDs 过滤规则：档位别名 / auto / default-<版本> / -sg / -x / -f 优先。
func TestFilterModelIDs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "别名与 auto",
			in:   []string{"auto", "default-model", "fast-model", "balanced-model", "primary-model", "deep-model", "glm-5.3"},
			want: []string{"glm-5.3"},
		},
		{
			name: "default 版本档",
			in:   []string{"default-1.1", "default-1.2", "glm-5.2"},
			want: []string{"glm-5.2"},
		},
		{
			name: "区域与变体后缀",
			in:   []string{"foo-sg", "bar-x", "hy3-x", "glm-5.3"},
			want: []string{"glm-5.3"},
		},
		{
			name: "-f 替换 base",
			in:   []string{"hy4-preview", "hy4-preview-f", "hy4-preview-dev"},
			want: []string{"hy4-preview-f", "hy4-preview-dev"},
		},
		{
			name: "只有 base 时保留",
			in:   []string{"hy4-preview", "glm-5.3"},
			want: []string{"hy4-preview", "glm-5.3"},
		},
		{
			name: "去重保序与去空串",
			in:   []string{" glm-5.3 ", "", "glm-5.3", "kimi-k3"},
			want: []string{"glm-5.3", "kimi-k3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterModelIDs(tc.in)
			if !catalogEquals(got, tc.want) {
				t.Fatalf("filterModelIDs(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for _, id := range got {
				if strings.TrimSpace(id) == "" {
					t.Fatalf("filter must never return empty id: %v", got)
				}
			}
		})
	}
}

// TestParseConfigModelsSourcePreference 源优先级与 cli agent 偏好。
func TestParseConfigModelsSourcePreference(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "cli agent 优先于非 cli",
			body: `{"data":{"agents":[{"name":"chat","models":["plain-a"]},{"agentType":"CLI","models":["cli-b"]}]}}`,
			want: []string{"cli-b"},
		},
		{
			name: "非 cli 回落",
			body: `{"data":{"agents":[{"name":"chat","models":["plain-a"]}]}}`,
			want: []string{"plain-a"},
		},
		{
			name: "agents 无模型回落 data.models",
			body: `{"data":{"agents":[{"name":"cli","models":["auto"]}],"models":["top-b"]}}`,
			want: []string{"top-b"},
		},
		{
			name: "data 缺失回落顶层 models",
			body: `{"models":["root-a"]}`,
			want: []string{"root-a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseConfigModels([]byte(tc.body))
			if !catalogEquals(got, tc.want) {
				t.Fatalf("parseConfigModels = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFetchModelsGarbageNeverPanics 两个端点返回非模型垃圾 → 快照兜底，不 panic。
func TestFetchModelsGarbageNeverPanics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"unexpected":{"nested":true},"items":[]}`))
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), srv.Client(), catalogProvider(srv.URL, RealmIntl), &oauth.TokenSet{})
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if len(models) == 0 {
		t.Fatalf("expected embedded snapshot fallback, got empty")
	}
}

// TestFetchModelsNotWorkBuddy 非 workbuddy 渠道返回 ErrNotWorkBuddy。
func TestFetchModelsNotWorkBuddy(t *testing.T) {
	p := &model.Provider{OAuthProvider: "kimi", BaseURL: "https://api.example.com"}
	_, err := FetchModels(context.Background(), http.DefaultClient, p, &oauth.TokenSet{})
	if !errors.Is(err, ErrNotWorkBuddy) {
		t.Fatalf("err = %v, want ErrNotWorkBuddy", err)
	}
	if _, err := FetchModels(context.Background(), http.DefaultClient, nil, nil); !errors.Is(err, ErrNotWorkBuddy) {
		t.Fatalf("nil provider err = %v, want ErrNotWorkBuddy", err)
	}
}

// TestCatalogErrorNeverLeaksToken 非 2xx 错误的错误信息绝不包含 token。
func TestCatalogErrorNeverLeaksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":12403,"msg":"bad desktop ua","error":{"message":"nope"}}`))
	}))
	defer srv.Close()

	p := catalogProvider(srv.URL, RealmIntl)
	tok := &oauth.TokenSet{AccessToken: "super-secret"}
	target, err := RemoteConfigEndpoint(p)
	if err != nil {
		t.Fatalf("RemoteConfigEndpoint: %v", err)
	}
	_, err = catalogGET(context.Background(), srv.Client(), target, p, tok)
	if err == nil {
		t.Fatalf("expected non-2xx error")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaks token: %v", err)
	}
}
