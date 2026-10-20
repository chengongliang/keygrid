package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// upstream_headers_test.go 渠道级上游附加请求头：校验（黑名单/CRLF/长度）
// + 落地语义（覆盖同名头、跳过被禁头）+ 失败分类里的「客户端版本过低」。

func TestValidUpstreamHeaders(t *testing.T) {
	cases := []struct {
		name    string
		in      map[string]string
		wantErr bool
	}{
		{"empty", nil, false},
		{"version header", map[string]string{"x-grok-client-version": "1.0.62"}, false},
		{"callback style", map[string]string{"x-custom-trace": "a-b_c.d"}, false},
		{"empty value allowed", map[string]string{"x-empty": ""}, false},
		{"blank entries dropped", map[string]string{" ": " "}, false},
		{"illegal name", map[string]string{"bad header": "v"}, true},
		{"illegal name colon", map[string]string{"x-a:b": "v"}, true},
		{"crlf value", map[string]string{"x-a": "v\r\nX-Evil: 1"}, true},
		{"too long value", map[string]string{"x-a": strings.Repeat("v", maxUpstreamHeaderValueLen+1)}, true},
		{"forbidden authorization", map[string]string{"Authorization": "Bearer x"}, true},
		{"forbidden case-insensitive", map[string]string{"aUtHoRiZaTiOn": "Bearer x"}, true},
		{"forbidden host", map[string]string{"Host": "evil.example"}, true},
		{"forbidden user-agent", map[string]string{"User-Agent": "x/1"}, true},
		{"forbidden xai token auth", map[string]string{"X-XAI-Token-Auth": "other"}, true},
		{"forbidden content-type", map[string]string{"Content-Type": "text/html"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidUpstreamHeaders(NormalizeUpstreamHeaders(tc.in))
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	// 超过条数上限
	tooMany := map[string]string{}
	for i := 0; i <= maxUpstreamHeaderCount; i++ {
		tooMany[fmt.Sprintf("x-h-%d", i)] = "v"
	}
	if err := ValidUpstreamHeaders(tooMany); err == nil {
		t.Fatalf("want error for %d headers", len(tooMany))
	}
}

func TestApplyUpstreamHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer platform")
	h.Set("x-grok-client-version", "1.0.62")

	p := &model.Provider{UpstreamHeaders: map[string]string{
		"x-grok-client-version":    "1.0.99",      // 覆盖默认值
		"X-Grok-Client-Identifier": "grok-cli",    // 新增
		"authorization":            "Bearer user", // 黑名单：跳过
		"bad header":               "v",           // 非法头名：跳过
	}}
	ApplyUpstreamHeaders(h, p)

	if got := h.Get("x-grok-client-version"); got != "1.0.99" {
		t.Fatalf("version = %q", got)
	}
	if got := h.Get("X-Grok-Client-Identifier"); got != "grok-cli" {
		t.Fatalf("identifier = %q", got)
	}
	if got := h.Get("Authorization"); got != "Bearer platform" {
		t.Fatalf("authorization must stay platform-owned, got %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}

	// nil 渠道 / 空附加头：不改动任何头（热路径零开销）
	before := h.Clone()
	ApplyUpstreamHeaders(h, nil)
	ApplyUpstreamHeaders(h, &model.Provider{})
	if fmt.Sprint(before) != fmt.Sprint(h) {
		t.Fatalf("headers changed without config")
	}
}

// 端到端：xAI 渠道附加头真正出现在上游请求里（覆盖内置版本号）。
func TestUpstreamCallXAIAppliesProviderHeaders(t *testing.T) {
	var gotVer, gotExtra, gotUA string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("x-grok-client-version")
		gotExtra = r.Header.Get("x-upstream-tweak")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.created","response":{"model":"grok-4.5"}}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
			``,
		}, "\n")))
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, err := BuildXAIRequest([]byte(`{"model":"grok","messages":[{"role":"user","content":"hi"}]}`), "grok-4.5")
	if err != nil {
		t.Fatalf("BuildXAIRequest: %v", err)
	}
	p := &model.Provider{
		Kind: "oauth", OAuthProvider: "xai",
		UpstreamHeaders: map[string]string{
			"x-grok-client-version": "9.9.9",
			"x-upstream-tweak":      "on",
		},
	}
	// 测试服务器不是官方 cli-chat-proxy host，身份头整体不注入；这里直接把渠道头
	// 作为唯一断言目标 —— applyUpstreamHeaders 对所有 endpoint 生效。
	if _, _, err := h.upstreamCallXAI(t.Context(), h.HTTPClient, up.URL+"/v1/responses", "tok", body, true, protoOpenAI, w, "", p); err != nil {
		t.Fatalf("upstreamCallXAI: %v", err)
	}
	if gotVer != "9.9.9" || gotExtra != "on" {
		t.Fatalf("provider headers not applied: ver=%q extra=%q", gotVer, gotExtra)
	}
	if strings.HasPrefix(gotUA, "xai-grok-workspace/") {
		t.Fatalf("custom endpoint must not receive grok identity UA, got %q", gotUA)
	}
}

func TestIsClientOutdatedErr(t *testing.T) {
	mk := func(status int, msg string) error {
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg}})
		return &UpstreamError{Status: status, Body: string(payload), ContentType: "application/json"}
	}
	cases := []struct {
		name   string
		status int
		err    error
		want   bool
	}{
		{"426 upgrade required", http.StatusUpgradeRequired, fmt.Errorf("upstream 426"), true},
		{"client version too old", http.StatusForbidden, mk(403, "client version 0.2.120 is too old, please upgrade"), true},
		{"unsupported cli version", http.StatusBadRequest, mk(400, "unsupported CLI version, upgrade required"), true},
		{"中文版本过低", http.StatusForbidden, mk(403, "客户端版本过低，请升级"), true},
		{"model version error stays 4xx", http.StatusBadRequest, mk(400, "model version not found"), false},
		{"plain 400", http.StatusBadRequest, mk(400, "invalid temperature"), false},
		{"5xx ignored", http.StatusInternalServerError, mk(500, "client version too old"), false},
		{"non upstream error", http.StatusBadRequest, fmt.Errorf("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientOutdatedErr(tc.status, tc.err); got != tc.want {
				t.Fatalf("isClientOutdatedErr = %v, want %v", got, tc.want)
			}
		})
	}
}

// rewriteToTransport 保留请求 URL 的 host（让官方 endpoint 判定生效），
// 只在 transport 层把请求转到本地 httptest 服务。
type rewriteToTransport struct{ base *url.URL }

func (t rewriteToTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.base.Scheme
	clone.URL.Host = t.base.Host
	clone.Host = t.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// 官方 endpoint：渠道只覆盖 x-grok-client-version 时，UA 版本同步跟随，
// 避免两个指纹头版本不一致。
func TestUpstreamCallXAISyncsUAWithVersionHeader(t *testing.T) {
	var gotVer, gotUA string
	var gotTokenAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVer = r.Header.Get("x-grok-client-version")
		gotUA = r.Header.Get("User-Agent")
		gotTokenAuth = r.Header.Get("X-XAI-Token-Auth")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
			``,
		}, "\n")))
	}))
	defer up.Close()
	base, err := url.Parse(up.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	h := &Handler{HTTPClient: &http.Client{Transport: rewriteToTransport{base: base}}}
	w := httptest.NewRecorder()
	body, err := BuildXAIRequest([]byte(`{"model":"grok","messages":[{"role":"user","content":"hi"}]}`), "grok-4.5")
	if err != nil {
		t.Fatalf("BuildXAIRequest: %v", err)
	}
	p := &model.Provider{
		Kind: "oauth", OAuthProvider: "xai",
		UpstreamHeaders: map[string]string{"x-grok-client-version": "9.9.9"},
	}
	if _, _, err := h.upstreamCallXAI(t.Context(), h.HTTPClient,
		"https://cli-chat-proxy.grok.com/v1/responses", "tok", body, true, protoOpenAI, w, "", p); err != nil {
		t.Fatalf("upstreamCallXAI: %v", err)
	}
	if gotVer != "9.9.9" {
		t.Fatalf("version header = %q", gotVer)
	}
	if gotUA != "xai-grok-workspace/9.9.9" {
		t.Fatalf("UA must follow version header, got %q", gotUA)
	}
	// 官方 host 仍注入 xAI 身份头（附加头机制不破坏内置身份头）
	if gotTokenAuth != xaiTokenAuthValue {
		t.Fatalf("token auth header = %q", gotTokenAuth)
	}
}
