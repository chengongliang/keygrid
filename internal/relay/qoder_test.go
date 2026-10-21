package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/qoder"
)

// qoder_test.go relay 侧 Qoder 转发：验证请求形态（COSY 签名 + WAF 编码 + 私有路径）
// 与响应回写（信封拆封成 OpenAI SSE / 折叠成非流式 JSON）。

// testCatalogBody 上游模型目录响应（含 model_config）。
const testCatalogBody = `{"chat":[
	{"key":"auto","display_name":"Auto","max_input_tokens":180000,"max_output_tokens":8192,
	 "context_config":[{"name":"200K","tokenCount":"200K","isDefault":true}]},
	{"key":"qmodel_latest","display_name":"Qwen3.7-Max","max_input_tokens":990000,"is_reasoning":true}
]}`

// qoderMock 记录收到的对话请求，并回放固定 SSE。
type qoderMock struct {
	mu          *testing.T
	chatCalls   int
	chatBody    []byte
	chatHeaders http.Header
	chatQuery   string
	sseLines    []string
}

// decodeQoderBody 反向解码 Qoder 的正文编码（仅测试用）：先把自定义字母表换回
// 标准 base64，再还原 [尾][中][头] → [头][中][尾]。
func decodeQoderBody(t *testing.T, encoded []byte) []byte {
	t.Helper()
	const std = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	const custom = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	back := map[byte]byte{'$': '='}
	for i := 0; i < len(std); i++ {
		back[custom[i]] = std[i]
	}
	rearranged := make([]byte, len(encoded))
	for i, c := range encoded {
		b, ok := back[c]
		if !ok {
			t.Fatalf("unexpected char %q in encoded body", c)
		}
		rearranged[i] = b
	}
	n := len(rearranged)
	a := n / 3
	// [尾][中][头] → [头][中][尾]
	var stdB64 []byte
	stdB64 = append(stdB64, rearranged[n-a:]...)
	stdB64 = append(stdB64, rearranged[a:n-a]...)
	stdB64 = append(stdB64, rearranged[:a]...)
	out, err := base64.StdEncoding.DecodeString(string(stdB64))
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

func newQoderMock(t *testing.T, sseLines []string) (*qoderMock, *httptest.Server) {
	t.Helper()
	m := &qoderMock{mu: t, sseLines: sseLines}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v2/model/list":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
				t.Errorf("model list missing COSY auth")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(testCatalogBody))
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			m.chatCalls++
			m.chatQuery = r.URL.RawQuery
			m.chatHeaders = r.Header.Clone()
			body, _ := io.ReadAll(r.Body)
			m.chatBody = body
			if got := r.Header.Get("Accept"); got != "text/event-stream" {
				t.Errorf("Accept = %q", got)
			}
			if got := r.Header.Get("Cosy-Sigpath"); got != qoder.ChatSigPath {
				t.Errorf("Cosy-Sigpath = %q", got)
			}
			if got := r.Header.Get("Accept-Encoding"); got != "identity" {
				t.Errorf("Accept-Encoding = %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, line := range m.sseLines {
				_, _ = w.Write([]byte("data: " + line + "\n\n"))
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return m, srv
}

func envelope(t *testing.T, inner string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// qoderTestProvider Qoder 渠道（api_key 形态：PAT / dt- 都能走本分支）。
func qoderTestProvider(base string) *model.Provider {
	return &model.Provider{
		ID:       1,
		Name:     "qoder-test",
		Kind:     "api_key",
		Protocol: "openai",
		BaseURL:  base,
	}
}

func TestUpstreamCallQoderStreamsOpenAISSE(t *testing.T) {
	m, srv := newQoderMock(t, []string{
		envelope(t, `{"id":"c1","model":"auto","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`),
		envelope(t, `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"content":"lo"}}]}`),
		envelope(t, `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"finish_reason":"stop"}}]}`),
		envelope(t, `{"id":"c1","model":"auto","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`),
		"[DONE]",
	})
	t.Setenv("QODER_CHAT_BASE", srv.URL)

	h := &Handler{HTTPClient: srv.Client(), Qoder: qoder.NewRuntime()}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"qoder/auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	status, usage, err := h.upstreamCallQoder(context.Background(), srv.Client(),
		qoderTestProvider(srv.URL), "dt-test", map[string]string{"userId": "u1", "machineId": "m1"},
		"auto", body, true, protoOpenAI, rec, "")
	if err != nil {
		t.Fatalf("upstreamCallQoder: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Fatalf("client stream missing content:\n%s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":11`) {
		t.Errorf("usage chunk not coalesced into stream:\n%s", out)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 4 || !usage.Found {
		t.Errorf("usage = %+v", usage)
	}

	// 上游请求形态：私有路径 + Encode=1 + 正文 WAF 编码可逆
	if m.chatCalls != 1 {
		t.Fatalf("chat calls = %d", m.chatCalls)
	}
	if !strings.Contains(m.chatQuery, "Encode=1") || !strings.Contains(m.chatQuery, "AgentId=agent_common") {
		t.Errorf("query = %q", m.chatQuery)
	}
	var payload map[string]any
	if err := json.Unmarshal(decodeQoderBody(t, m.chatBody), &payload); err != nil {
		t.Fatalf("decoded payload: %v", err)
	}
	if payload["session_type"] != "qodercli" || payload["system"] != "" {
		t.Errorf("payload envelope = %v", payload)
	}
	if _, ok := payload["model_config"].(map[string]any); !ok {
		t.Errorf("payload missing model_config: %v", payload)
	}
	if got := m.chatHeaders.Get("X-Model-Key"); got != "auto" {
		t.Errorf("X-Model-Key = %q", got)
	}
	// COSY 正文签名与Bodyhash 必须针对编码后的字节
	if got := m.chatHeaders.Get("Cosy-Bodylength"); got != itoa(len(m.chatBody)) {
		t.Errorf("Cosy-Bodylength = %q, want %d", got, len(m.chatBody))
	}
	if !strings.HasPrefix(m.chatHeaders.Get("Authorization"), "Bearer COSY.") {
		t.Errorf("missing COSY authorization")
	}
}

func TestUpstreamCallQoderCollapsesForNonStream(t *testing.T) {
	m, srv := newQoderMock(t, []string{
		envelope(t, `{"id":"c1","model":"auto","created":1700000000,"choices":[{"index":0,"delta":{"content":"Hello "}}]}`),
		envelope(t, `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"content":"world"}}]}`),
		envelope(t, `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"finish_reason":"stop"}}]}`),
		envelope(t, `{"id":"c1","model":"auto","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`),
		"[DONE]",
	})
	t.Setenv("QODER_CHAT_BASE", srv.URL)

	h := &Handler{HTTPClient: srv.Client(), Qoder: qoder.NewRuntime()}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"qoder/auto","messages":[{"role":"user","content":"hi"}]}`)

	status, usage, err := h.upstreamCallQoder(context.Background(), srv.Client(),
		qoderTestProvider(srv.URL), "dt-test", map[string]string{"userId": "u1"},
		"auto", body, false, protoOpenAI, rec, "")
	if err != nil {
		t.Fatalf("upstreamCallQoder: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("non-stream body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hello world" {
		t.Fatalf("choices = %+v", resp.Choices)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 5 || usage.PromptTokens != 5 {
		t.Errorf("usage = %+v / %+v", resp.Usage, usage)
	}
	if m.chatCalls != 1 {
		t.Errorf("chat calls = %d", m.chatCalls)
	}
}

func TestUpstreamCallQoderFirstFrameErrorSurfacesStatus(t *testing.T) {
	_, srv := newQoderMock(t, []string{
		`{"statusCodeValue":403,"body":"{\"code\":\"110\",\"message\":\"daily billing limit\"}"}`,
	})
	t.Setenv("QODER_CHAT_BASE", srv.URL)

	h := &Handler{HTTPClient: srv.Client(), Qoder: qoder.NewRuntime()}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	status, _, err := h.upstreamCallQoder(context.Background(), srv.Client(),
		qoderTestProvider(srv.URL), "dt-test", map[string]string{"userId": "u1"},
		"auto", body, true, protoOpenAI, rec, "")
	// 计费/配额拦截按 429 上报（failover 语义），而不是上游原始的 403。
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	ue, ok := err.(*UpstreamError)
	if !ok || ue.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ue.Body, "billing") {
		t.Errorf("err body = %q", ue.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("no body should be written before the error status: %q", rec.Body.String())
	}
}

func TestUpstreamCallQoderMissingUserID(t *testing.T) {
	// 没有 userId 又拿不到 userinfo（mock 返回 401）→ 401 语义，提示重新授权
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("QODER_CHAT_BASE", srv.URL)
	t.Setenv("QODER_USERINFO_URL", srv.URL+"/api/v1/userinfo")

	h := &Handler{HTTPClient: srv.Client(), Qoder: qoder.NewRuntime()}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	status, _, err := h.upstreamCallQoder(context.Background(), srv.Client(),
		qoderTestProvider(srv.URL), "dt-test", nil,
		"auto", body, true, protoOpenAI, rec, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if err == nil || !strings.Contains(err.Error(), "userId") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpstreamCallQoderPATEndToEnd(t *testing.T) {
	// PAT：先兑换 jt-，再签名；国际站 jt- 必须走 api2（这里用同一 mock，只验证兑换被调用）
	var exchangeCalls int
	m, srv := newQoderMock(t, []string{
		envelope(t, `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"content":"ok"}}]}`),
		envelope(t, `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"finish_reason":"stop"}}]}`),
		"[DONE]",
	})
	// 覆盖 jobToken 端点：复用同一 server 的 handler 前先包一层
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/jobToken/exchange" {
			exchangeCalls++
			_, _ = w.Write([]byte(`{"token":"jt-1","expires_in":86400}`))
			return
		}
		if r.URL.Path == "/api/v1/userinfo" {
			_, _ = w.Write([]byte(`{"id":"u-pat"}`))
			return
		}
		inner.ServeHTTP(w, r)
	})
	t.Setenv("QODER_CHAT_BASE", srv.URL)
	t.Setenv("QODER_JOB_TOKEN_URL", srv.URL+"/api/v1/jobToken/exchange")
	t.Setenv("QODER_USERINFO_URL", srv.URL+"/api/v1/userinfo")

	h := &Handler{HTTPClient: srv.Client(), Qoder: qoder.NewRuntime()}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	status, _, err := h.upstreamCallQoder(context.Background(), srv.Client(),
		qoderTestProvider(srv.URL), "pt-secret", nil,
		"auto", body, true, protoOpenAI, rec, "")
	if err != nil {
		t.Fatalf("upstreamCallQoder: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if exchangeCalls != 1 {
		t.Errorf("PAT exchange calls = %d", exchangeCalls)
	}
	if m.chatCalls != 1 {
		t.Fatalf("chat calls = %d", m.chatCalls)
	}
	// 签名用的必须是兑换来的 jt-（Cosy-User 来自 userinfo）
	if got := m.chatHeaders.Get("Cosy-User"); got != "u-pat" {
		t.Errorf("Cosy-User = %q", got)
	}
	if !strings.Contains(string(decodeQoderBody(t, m.chatBody)), `"chat_task":"FREE_INPUT"`) {
		t.Errorf("payload not encoded/decoded as expected")
	}
}

// itoa 小工具（避免测试里引 strconv）。
func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestChannelProtoForcesOpenAIForQoder(t *testing.T) {
	// 即使协议字段被误填成 anthropic，Qoder 也必须走私有 /algo 分支，
	// 否则会拼出不存在的 /v1/messages。
	cases := []*model.Provider{
		{Kind: "api_key", Protocol: "anthropic", BaseURL: "https://api3.qoder.sh"},
		{Kind: "oauth", Protocol: "anthropic", OAuthProvider: "qoder-cn", BaseURL: "https://gateway.qoder.com.cn"},
	}
	for i, p := range cases {
		if got := channelProto(p); got != protoOpenAI {
			t.Errorf("case %d: channelProto = %q, want openai", i, got)
		}
	}
	// 非 Qoder 的 anthropic 渠道不受影响
	if got := channelProto(&model.Provider{Protocol: "anthropic", BaseURL: "https://api.anthropic.com"}); got != protoAnthropic {
		t.Errorf("anthropic provider changed behavior: %q", got)
	}
}
