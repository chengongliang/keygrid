package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// workbuddy_test.go WorkBuddy 渠道在 relay 侧的接线契约。
//
// 覆盖三件在真实上游上才会暴露的事：
//  1. 额外身分头确实发到了上游（缺它们上游 WAF 直接 11128）；
//  2. 上游只能流式时，即使客户端要非流式也按 SSE 收（Accept: text/event-stream）；
//  3. 非流式客户端拿到的是折叠后的单条 JSON，而不是 SSE 原文，
//     且 usage 能被记账链路取到。

const wbSSEUpstream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}

data: [DONE]

`

func TestUpstreamCallWorkbuddyForcedStreamCollapse(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(wbSSEUpstream))
	}))
	defer srv.Close()

	extra := http.Header{}
	extra.Set("X-IDE-Type", "WorkBuddy")
	extra.Set("X-IDE-Name", "WorkBuddy")
	extra.Set("X-CodeBuddy-Request", "1")
	extra.Set("User-Agent", "WorkBuddy/from-extra")
	extra.Set("Content-Type", "application/json")
	extra.Set("Accept", "application/json")

	h := &Handler{HTTPClient: srv.Client()}
	rec := httptest.NewRecorder()
	status, usage, err := h.upstreamCall(context.Background(), srv.Client(), srv.URL, "tok-1",
		[]byte(`{"model":"m","stream":false}`), false, protoOpenAI, rec, "WorkBuddy/from-ua", extra, true)
	if err != nil {
		t.Fatalf("upstreamCall error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	// 1. 身分头必须到达上游
	for _, k := range []string{"X-Ide-Type", "X-Ide-Name", "X-Codebuddy-Request"} {
		if got.Get(k) == "" {
			t.Fatalf("上游未收到身分头 %s（headers=%v）", k, got)
		}
	}
	if got.Get("Authorization") != "Bearer tok-1" {
		t.Fatalf("Authorization = %q", got.Get("Authorization"))
	}
	// 2. UA 以调用方传入的 ua 参数为准（渠道 ua_mode 语义），不被额外头覆盖
	if ua := got.Get("User-Agent"); ua != "WorkBuddy/from-ua" {
		t.Fatalf("User-Agent = %q, want WorkBuddy/from-ua", ua)
	}
	// 3. 强制流式：Accept 必须是 SSE，即使客户端要非流式
	if acc := got.Get("Accept"); acc != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", acc)
	}

	// 4. 客户端拿到的是折叠后的 JSON
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := rec.Body.Bytes()
	if strings.Contains(string(body), "data:") {
		t.Fatalf("响应里还有 SSE 原文: %s", body)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, body)
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Content != "pong" {
		t.Fatalf("折叠结果不对: %s", body)
	}
	// 5. 记账：折叠保留的 usage 必须被提取到（否则这类请求全部 0 token）
	if !usage.Found || usage.PromptTokens != 5 || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v, want prompt=5 completion=2 found=true", usage)
	}
}

func TestUpstreamCallWorkbuddyStreamPassthrough(t *testing.T) {
	// 客户端要流式时不应走折叠：SSE 原样透传
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(wbSSEUpstream))
	}))
	defer srv.Close()

	h := &Handler{HTTPClient: srv.Client()}
	rec := httptest.NewRecorder()
	status, _, err := h.upstreamCall(context.Background(), srv.Client(), srv.URL, "tok", nil,
		true, protoOpenAI, rec, "", nil, true)
	if err != nil {
		t.Fatalf("upstreamCall error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("流式响应应原样透传: %s", rec.Body.String())
	}
}

func TestUpstreamCallNonWorkbuddyUnaffected(t *testing.T) {
	// 回归：extra=nil / forceStream=false（所有既有渠道）时行为与改动前一致：
	// Accept 跟随客户端，响应字节透传。
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	h := &Handler{HTTPClient: srv.Client()}
	rec := httptest.NewRecorder()
	status, usage, err := h.upstreamCall(context.Background(), srv.Client(), srv.URL, "tok", nil,
		false, protoOpenAI, rec, "", nil, false)
	if err != nil || status != http.StatusOK {
		t.Fatalf("upstreamCall status=%d err=%v", status, err)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("非 workbuddy 渠道响应应原样透传: %s", rec.Body.String())
	}
	if !usage.Found || usage.PromptTokens != 1 {
		t.Fatalf("usage = %+v", usage)
	}
}
