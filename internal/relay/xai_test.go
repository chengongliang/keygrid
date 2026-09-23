package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

func TestXAIProviderAndBaseURL(t *testing.T) {
	if !IsXAIProvider(&model.Provider{Kind: "oauth", OAuthProvider: "xai"}) {
		t.Fatal("xai oauth provider not detected")
	}
	if !IsResponsesProvider(&model.Provider{Kind: "oauth", OAuthProvider: "xai"}) {
		t.Fatal("xai provider must use Responses")
	}
	if IsXAIProvider(&model.Provider{Kind: "api_key", OAuthProvider: "xai"}) {
		t.Fatal("api_key provider must not be treated as xai oauth")
	}
	cases := map[string]string{
		"https://cli-chat-proxy.grok.com/v1":           "https://cli-chat-proxy.grok.com/v1/responses",
		"https://cli-chat-proxy.grok.com/v1/responses": "https://cli-chat-proxy.grok.com/v1/responses",
		"https://api.x.ai":                             "https://api.x.ai/v1/responses",
		"https://relay.example/v1/responses/":          "https://relay.example/v1/responses",
		"  https://relay.example/grok  ":               "https://relay.example/grok/responses",
	}
	for in, want := range cases {
		if got := NormalizeXAIBaseURL(in); got != want {
			t.Errorf("NormalizeXAIBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildXAIRequestRestoresResponsesControls(t *testing.T) {
	body, err := BuildXAIRequest([]byte(`{
		"model":"grok-client",
		"messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hello"}],
		"max_tokens":17,"temperature":0.2,"stop":["done"],"stream":false
	}`), "grok-4.5")
	if err != nil {
		t.Fatalf("BuildXAIRequest: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got["model"] != "grok-4.5" || got["stream"] != true || got["store"] != false {
		t.Fatalf("request controls: %v", got)
	}
	if got["max_output_tokens"] != float64(17) || got["temperature"] != float64(0.2) {
		t.Fatalf("xAI controls not restored: %v", got)
	}
	if _, ok := got["stop"]; ok {
		t.Fatal("stop is not supported by xAI Responses")
	}
	if got["instructions"] != "be concise" {
		t.Fatalf("system message must become instructions: %v", got["instructions"])
	}
}

func TestXAIUpstreamHeadersOfficialOnly(t *testing.T) {
	official := XAIUpstreamHeaders("tok", "https://cli-chat-proxy.grok.com/v1/responses", "conv-1")
	for key, want := range map[string]string{
		xaiTokenAuthHeader:     xaiTokenAuthValue,
		xaiClientVersionHeader: xaiClientVersion,
		xaiClientIDHeader:      xaiClientIDValue,
		xaiAuthResponseHeader:  xaiAuthResponseValue,
		"User-Agent":           "xai-grok-workspace/" + xaiClientVersion,
		xaiConversationHeader:  "conv-1",
	} {
		if got := official.Get(key); got != want {
			t.Errorf("official header %s = %q, want %q", key, got, want)
		}
	}
	custom := XAIUpstreamHeaders("tok", "https://relay.example/v1/responses", "")
	if custom.Get(xaiTokenAuthHeader) != "" || custom.Get(xaiClientVersionHeader) != "" {
		t.Fatalf("custom endpoint received Grok Build headers: %v", custom)
	}
}

func TestUpstreamCallXAIStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok-xai" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get(xaiTokenAuthHeader) != "" {
			t.Errorf("test custom endpoint should not receive official header: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.created","response":{"model":"grok-4.5"}}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":2}}}`,
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
	status, usage, err := h.upstreamCallXAI(t.Context(), h.HTTPClient, up.URL+"/v1/responses", "tok-xai", body, true, protoOpenAI, w, "")
	if err != nil {
		t.Fatalf("upstreamCallXAI: %v", err)
	}
	if status != http.StatusOK || usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Fatalf("status/usage = %d/%+v", status, usage)
	}
	if !strings.Contains(w.Body.String(), "chat.completion.chunk") || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("unexpected client stream: %s", w.Body.String())
	}
}
