package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// ---- 请求转换：/v1/messages → pivot ----

func TestAnthropicToPivotRequestShape(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 512,
		"system": "be terse",
		"stream": true,
		"temperature": 0.7,
		"stop_sequences": ["END"],
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": " ponder"},
				{"type": "text", "text": "Hi there"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "hf"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "18C"},
				{"type": "text", "text": "and now?"}
			]},
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGk="}}
			]}
		],
		"tools": [{"name": "get_weather", "description": "w", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"tool_choice": {"type": "any"}
	}`

	pivot, modelName, stream, err := anthropicToPivotRequest([]byte(body))
	if err != nil {
		t.Fatalf("anthropicToPivotRequest: %v", err)
	}
	if modelName != "claude-sonnet-4-5" || !stream {
		t.Fatalf("model=%q stream=%v", modelName, stream)
	}

	var req map[string]any
	if err := json.Unmarshal(pivot, &req); err != nil {
		t.Fatalf("pivot json: %v", err)
	}
	if req["max_tokens"] != float64(512) || req["temperature"] != 0.7 || req["stream"] != true {
		t.Fatalf("params: %v", req)
	}
	stop, _ := req["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop: %v", req["stop"])
	}

	msgs, _ := req["messages"].([]any)
	// system + user + assistant(tool) + tool + user(text) + user(image) = 6
	if len(msgs) != 6 {
		t.Fatalf("expected 6 pivot messages, got %d: %v", len(msgs), req["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be terse" {
		t.Fatalf("system message: %v", m0)
	}
	// assistant：text + tool_calls + reasoning_content
	m2, _ := msgs[2].(map[string]any)
	if m2["role"] != "assistant" || m2["content"] != "Hi there" || m2["reasoning_content"] != " ponder" {
		t.Fatalf("assistant message: %v", m2)
	}
	tcs, _ := m2["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls: %v", m2["tool_calls"])
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" {
		t.Fatalf("tool id: %v", tc)
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"hf"}` {
		t.Fatalf("tool function: %v", fn)
	}
	// tool_result → role=tool
	m3, _ := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "toolu_1" || m3["content"] != "18C" {
		t.Fatalf("tool message: %v", m3)
	}
	// image base64 → data URL（单 part 展开为对象）
	m5, _ := msgs[5].(map[string]any)
	img, _ := m5["content"].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image part: %v", m5["content"])
	}
	iu, _ := img["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,aGk=" {
		t.Fatalf("image url: %v", iu)
	}

	// tools / tool_choice
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %v", req["tools"])
	}
	t0, _ := tools[0].(map[string]any)
	if t0["type"] != "function" {
		t.Fatalf("tool type: %v", t0)
	}
	if req["tool_choice"] != "required" {
		t.Fatalf("tool_choice any → required, got %v", req["tool_choice"])
	}
}

func TestAnthropicToPivotRequestValidation(t *testing.T) {
	if _, _, _, err := anthropicToPivotRequest([]byte(`{"max_tokens":10}`)); err == nil {
		t.Fatal("model required")
	}
	if _, _, _, err := anthropicToPivotRequest([]byte(`not json`)); err == nil {
		t.Fatal("invalid json must error")
	}
	// system blocks 数组形状
	pivot, _, _, err := anthropicToPivotRequest([]byte(
		`{"model":"m","system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[]}`))
	if err != nil {
		t.Fatalf("system blocks: %v", err)
	}
	var req map[string]any
	_ = json.Unmarshal(pivot, &req)
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("system blocks → 1 system message, got %v", msgs)
	}
	if m0, _ := msgs[0].(map[string]any); m0["content"] != "a\n\nb" {
		t.Fatalf("system join: %v", m0)
	}
}

// ---- 请求转换：pivot → /v1/messages 上游 ----

func TestPivotToAnthropicRequestShape(t *testing.T) {
	pivot := `{
		"model": "m",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "developer", "content": "extra sys"},
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": null, "reasoning_content": "dropped",
			 "tool_calls": [{"id": "call_1", "type": "function",
				"function": {"name": "get_weather", "arguments": "{\"city\":\"hf\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "18C"},
			{"role": "tool", "tool_call_id": "call_2", "content": "rainy"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "w",
			"parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}],
		"tool_choice": "required",
		"stop": ["END"],
		"temperature": 0.5,
		"max_tokens": 256
	}`

	up, err := pivotToAnthropicRequest([]byte(pivot), "up-model")
	if err != nil {
		t.Fatalf("pivotToAnthropicRequest: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(up, &req); err != nil {
		t.Fatalf("upstream json: %v", err)
	}
	if req["model"] != "up-model" || req["max_tokens"] != float64(256) {
		t.Fatalf("model/max_tokens: %v", req)
	}
	if req["system"] != "be terse\n\nextra sys" {
		t.Fatalf("system: %v", req["system"])
	}
	msgs, _ := req["messages"].([]any)
	// user + assistant(tool_use) + user(2×tool_result) = 3
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %v", req["messages"])
	}
	m1, _ := msgs[1].(map[string]any)
	if m1["role"] != "assistant" {
		t.Fatalf("assistant: %v", m1)
	}
	blocks, _ := m1["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks (reasoning dropped): %v", blocks)
	}
	b0, _ := blocks[0].(map[string]any)
	if b0["type"] != "tool_use" || b0["id"] != "call_1" || b0["name"] != "get_weather" {
		t.Fatalf("tool_use block: %v", b0)
	}
	inp, ok := b0["input"].(map[string]any)
	if !ok || inp["city"] != "hf" {
		t.Fatalf("tool input: %v", b0["input"])
	}
	// 连续 tool 消息合并进同一条 user
	m2, _ := msgs[2].(map[string]any)
	if m2["role"] != "user" {
		t.Fatalf("merged user: %v", m2)
	}
	trs, _ := m2["content"].([]any)
	if len(trs) != 2 {
		t.Fatalf("tool_result blocks: %v", trs)
	}
	// tools / tool_choice / stop
	tools, _ := req["tools"].([]any)
	t0, _ := tools[0].(map[string]any)
	if t0["name"] != "get_weather" {
		t.Fatalf("anthropic tool: %v", t0)
	}
	if _, ok := t0["input_schema"].(map[string]any); !ok {
		t.Fatalf("input_schema: %v", t0)
	}
	ch, _ := req["tool_choice"].(map[string]any)
	if ch["type"] != "any" {
		t.Fatalf("tool_choice: %v", req["tool_choice"])
	}
	ss, _ := req["stop_sequences"].([]any)
	if len(ss) != 1 || ss[0] != "END" {
		t.Fatalf("stop_sequences: %v", req["stop_sequences"])
	}
}

func TestPivotToAnthropicMaxTokensDefault(t *testing.T) {
	up, err := pivotToAnthropicRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(up, &req)
	if req["max_tokens"] != float64(4096) {
		t.Fatalf("default max_tokens: %v", req["max_tokens"])
	}
}

// ---- 响应转换：anthropic JSON → relayAgg → 各格式 ----

func TestAnthropicJSONToAgg(t *testing.T) {
	data := []byte(`{
		"id": "msg_x", "type": "message", "role": "assistant", "model": "claude",
		"content": [
			{"type": "thinking", "thinking": "think"},
			{"type": "text", "text": "Weather is "},
			{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "hf"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 11, "output_tokens": 22}
	}`)
	agg, ok := anthropicJSONToAgg(data, "claude")
	if !ok {
		t.Fatal("parse failed")
	}
	if agg.Content != "Weather is " || agg.Reasoning != "think" {
		t.Fatalf("content/reasoning: %+v", agg)
	}
	if !agg.HasToolCall || len(agg.ToolCalls) != 1 {
		t.Fatalf("tool calls: %+v", agg)
	}
	if agg.ToolCalls[0].ID != "toolu_1" || agg.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("tool call: %+v", agg.ToolCalls[0])
	}
	if agg.ToolCalls[0].Arguments != `{"city":"hf"}` {
		t.Fatalf("arguments: %q", agg.ToolCalls[0].Arguments)
	}
	if agg.FinishReason != "tool_calls" {
		t.Fatalf("finish: %q", agg.FinishReason)
	}
	if agg.PromptTokens != 11 || agg.CompletionTokens != 22 {
		t.Fatalf("usage: %+v", agg)
	}
}

func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"weird":         "stop",
	}
	for in, want := range cases {
		if got := finishFromAnthropicStop(in); got != want {
			t.Fatalf("finishFromAnthropicStop(%q)=%q want %q", in, got, want)
		}
	}
	back := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "": "end_turn",
	}
	for in, want := range back {
		if got := anthropicStopFromFinish(in); got != want {
			t.Fatalf("anthropicStopFromFinish(%q)=%q want %q", in, got, want)
		}
	}
}

// ---- 上游调用：anthropic 渠道 ----

func TestUpstreamCallAnthropicNonStreamChatEntry(t *testing.T) {
	var gotAPIKey, gotVersion string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_u", "type": "message", "role": "assistant", "model": "up-model",
			"content": [{"type": "text", "text": "hello from anthropic"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 5, "output_tokens": 7}
		}`))
	}))
	defer up.Close()

	pivot, _, _, _ := anthropicToPivotRequest([]byte(
		`{"model":"claude-4","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	upBody, _ := pivotToAnthropicRequest(pivot, "up-model")

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	p := &model.Provider{Kind: "api_key", Protocol: "anthropic"}
	status, usage, err := h.upstreamCallAnthropic(t.Context(), h.HTTPClient, up.URL+"/v1/messages", p,
		"sk-up", upBody, false, protoOpenAI, w)
	if err != nil {
		t.Fatalf("upstreamCallAnthropic: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if gotAPIKey != "sk-up" || gotVersion != anthropicVersion {
		t.Fatalf("headers: api-key=%q version=%q", gotAPIKey, gotVersion)
	}
	if gotBody["model"] != "up-model" || gotBody["max_tokens"] != float64(100) {
		t.Fatalf("upstream body: %v", gotBody)
	}
	if usage.PromptTokens != 5 || usage.CompletionTokens != 7 || !usage.Found {
		t.Fatalf("usage: %+v", usage)
	}

	// 客户端收到 chat.completion JSON
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body: %s", w.Body.String())
	}
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	if msg["content"] != "hello from anthropic" {
		t.Fatalf("message: %v", msg)
	}
	if c0["finish_reason"] != "stop" {
		t.Fatalf("finish: %v", c0["finish_reason"])
	}
	u, _ := resp["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(5) || u["completion_tokens"] != float64(7) {
		t.Fatalf("client usage: %v", u)
	}
}

func TestUpstreamCallAnthropicMessagesPassthrough(t *testing.T) {
	// 入口 messages + anthropic 渠道：非流式字节透传、流式 SSE 透传 + 记账
	upSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"up-model","usage":{"input_tokens":9}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-beta") != "" {
			t.Errorf("non-oauth channel must not send anthropic-beta")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upSSE))
	}))
	defer up.Close()

	h := NewHandler(nil)

	// 流式透传 + 记账（input/output tokens）
	w := httptest.NewRecorder()
	upBody := []byte(`{"model":"up-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	p := &model.Provider{Kind: "api_key", Protocol: "anthropic"}
	status, usage, err := h.upstreamCallAnthropic(t.Context(), h.HTTPClient, up.URL, p,
		"sk-up", upBody, true, protoAnthropic, w)
	if err != nil {
		t.Fatalf("stream passthrough: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if usage.PromptTokens != 9 || usage.CompletionTokens != 3 || !usage.Found {
		t.Fatalf("anthropic usage extraction: %+v", usage)
	}
	if !strings.Contains(w.Body.String(), `"type":"content_block_delta"`) {
		t.Fatalf("raw SSE must pass through: %s", w.Body.String())
	}
}

func TestAnthropicUpstreamHeadersOAuth(t *testing.T) {
	// Claude Pro oauth：Bearer + anthropic-beta；kimi oauth：Bearer + x-api-key
	h1 := anthropicUpstreamHeaders(&model.Provider{Kind: "oauth", OAuthProvider: "anthropic"}, "tok")
	if h1.Get("Authorization") != "Bearer tok" || h1.Get("anthropic-beta") != "oauth-2025-04-20" {
		t.Fatalf("anthropic oauth headers: %v", h1)
	}
	if h1.Get("x-api-key") != "" {
		t.Fatalf("anthropic oauth must not set x-api-key: %v", h1)
	}
	h2 := anthropicUpstreamHeaders(&model.Provider{Kind: "oauth", OAuthProvider: "kimi"}, "tok")
	if h2.Get("Authorization") != "Bearer tok" || h2.Get("x-api-key") != "tok" {
		t.Fatalf("kimi oauth headers: %v", h2)
	}
	h3 := anthropicUpstreamHeaders(&model.Provider{Kind: "api_key"}, "key")
	if h3.Get("x-api-key") != "key" || h3.Get("Authorization") != "" {
		t.Fatalf("api_key headers: %v", h3)
	}
}

func TestChannelProto(t *testing.T) {
	if got := channelProto(&model.Provider{Kind: "oauth", OAuthProvider: "openai"}); got != protoResponses {
		t.Fatalf("codex → responses, got %s", got)
	}
	if got := channelProto(&model.Provider{Kind: "api_key", Protocol: "anthropic"}); got != protoAnthropic {
		t.Fatalf("anthropic, got %s", got)
	}
	if got := channelProto(&model.Provider{Kind: "api_key", Protocol: "openai"}); got != protoOpenAI {
		t.Fatalf("default openai, got %s", got)
	}
}

func TestAnthropicToolChoiceDroppedWithoutTools(t *testing.T) {
	// 入口方向（anthropic → pivot）：tools 缺失时 tool_choice 一并剔除
	pivot, _, _, err := anthropicToPivotRequest([]byte(`{
		"model": "m", "max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}],
		"tool_choice": {"type": "auto"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(pivot, &req)
	if _, ok := req["tool_choice"]; ok {
		t.Fatalf("pivot tool_choice must be dropped without tools: %v", req["tool_choice"])
	}

	// 渠道方向（pivot → anthropic）：同样剔除
	up, err := pivotToAnthropicRequest([]byte(`{
		"model": "m", "max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}],
		"tool_choice": "required"
	}`), "up-model")
	if err != nil {
		t.Fatal(err)
	}
	var upReq map[string]any
	_ = json.Unmarshal(up, &upReq)
	if _, ok := upReq["tool_choice"]; ok {
		t.Fatalf("upstream tool_choice must be dropped without tools: %v", upReq["tool_choice"])
	}

	// 对照：有 tools 时 tool_choice 保留
	up2, err := pivotToAnthropicRequest([]byte(`{
		"model": "m", "max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {"name": "shell", "parameters": {"type": "object"}}}],
		"tool_choice": "required"
	}`), "up-model")
	if err != nil {
		t.Fatal(err)
	}
	var keep map[string]any
	_ = json.Unmarshal(up2, &keep)
	tc, _ := keep["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "any" {
		t.Fatalf("tool_choice must be kept with tools: %v", keep["tool_choice"])
	}
}
