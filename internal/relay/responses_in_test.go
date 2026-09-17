package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- 请求转换：/v1/responses → pivot ----

// Codex 客户端的 developer 消息经 pivot 后不得原样进入 OpenAI 兼容上游请求
// （DeepSeek 等只认 system/user/assistant/tool，收到 developer 会 400 拒绝）。
func TestResponsesDeveloperRoleNormalizedForOpenAIUpstream(t *testing.T) {
	pivot, _, _, err := responsesToPivotRequest([]byte(`{
		"model": "m",
		"input": [
			{"type": "message", "role": "developer", "content": "env ctx"},
			{"type": "message", "role": "user", "content": "hi"}
		]
	}`))
	if err != nil {
		t.Fatalf("responsesToPivotRequest: %v", err)
	}
	up := buildUpstreamBody(pivot, "m", "m")
	var body map[string]any
	if err := json.Unmarshal(up, &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages: %v", msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer must be normalized to system: %v", msgs)
	}
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("user role must survive: %v", msgs)
	}
}

func TestResponsesToPivotRequestShape(t *testing.T) {
	body := `{
		"model": "gpt-5.4",
		"instructions": "be terse",
		"stream": true,
		"input": [
			{"type": "message", "role": "user", "content": "hello"},
			{"type": "message", "role": "user", "content": [
				{"type": "input_text", "text": "look "},
				{"type": "input_image", "image_url": "https://x/y.png"}
			]},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"hf\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "18C"},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "hmm"}]}
		],
		"tools": [
			{"type": "function", "name": "get_weather", "description": "w",
			 "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}},
			{"type": "web_search"}
		],
		"tool_choice": {"type": "function", "name": "get_weather"},
		"max_output_tokens": 300,
		"temperature": 0.2,
		"reasoning": {"effort": "high", "summary": "auto"},
		"store": false,
		"previous_response_id": "resp_old"
	}`

	pivot, modelName, stream, err := responsesToPivotRequest([]byte(body))
	if err != nil {
		t.Fatalf("responsesToPivotRequest: %v", err)
	}
	if modelName != "gpt-5.4" || !stream {
		t.Fatalf("model=%q stream=%v", modelName, stream)
	}

	var req map[string]any
	if err := json.Unmarshal(pivot, &req); err != nil {
		t.Fatalf("pivot json: %v", err)
	}
	// 服务端状态参数丢弃、max_output_tokens → max_tokens、reasoning.effort → reasoning_effort
	if _, ok := req["store"]; ok {
		t.Fatalf("store must be dropped: %v", req)
	}
	if _, ok := req["previous_response_id"]; ok {
		t.Fatalf("previous_response_id must be dropped")
	}
	if req["max_tokens"] != float64(300) || req["reasoning_effort"] != "high" || req["temperature"] != 0.2 {
		t.Fatalf("params: %v", req)
	}

	msgs, _ := req["messages"].([]any)
	// system(instructions) + user + user(2 parts) + assistant(tool_calls) + tool = 5
	if len(msgs) != 5 {
		t.Fatalf("expected 5 pivot messages, got %d: %v", len(msgs), req["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be terse" {
		t.Fatalf("instructions → system: %v", m0)
	}
	// 图片 part
	m2, _ := msgs[2].(map[string]any)
	parts, _ := m2["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user parts: %v", m2["content"])
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image part: %v", img)
	}
	// function_call item → assistant tool_calls
	m3, _ := msgs[3].(map[string]any)
	tcs, _ := m3["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls: %v", m3)
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["id"] != "call_1" || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"hf"}` {
		t.Fatalf("function_call item: %v", tc)
	}
	// function_call_output item → tool 消息
	m4, _ := msgs[4].(map[string]any)
	if m4["role"] != "tool" || m4["tool_call_id"] != "call_1" || m4["content"] != "18C" {
		t.Fatalf("function_call_output item: %v", m4)
	}
	// tools：function 扁平→嵌套；web_search 等 chat 无法表达的类型被丢弃
	// （透传会被上游参数校验 400 拒绝整条请求）
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools (web_search should be dropped): %v", req["tools"])
	}
	t0, _ := tools[0].(map[string]any)
	fn0, _ := t0["function"].(map[string]any)
	if fn0["name"] != "get_weather" {
		t.Fatalf("nested tool: %v", t0)
	}
	// tool_choice
	tc2, _ := req["tool_choice"].(map[string]any)
	fn2, _ := tc2["function"].(map[string]any)
	if fn2["name"] != "get_weather" {
		t.Fatalf("tool_choice: %v", req["tool_choice"])
	}
}

func TestResponsesToPivotShorthandMessage(t *testing.T) {
	pivot, _, _, err := responsesToPivotRequest([]byte(`{
		"model":"m",
		"input":[{"role":"user","content":"hello"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(pivot, &req)
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("shorthand input must be preserved: %v", msgs)
	}
	m, _ := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hello" {
		t.Fatalf("shorthand message: %v", m)
	}
}

func TestResponsesToPivotDropsToolChoiceWithoutTools(t *testing.T) {
	// codex 特有工具（local_shell 等）无法映射到 chat,tools 全部被丢弃后
	// tool_choice 必须一并剔除,否则 OpenAI 兼容上游 400:
	// "When using tool_choice, tools must be set"
	for _, tc := range []string{
		`{"model":"m","input":"hi","tools":[{"type":"local_shell"}],"tool_choice":"auto"}`,
		// 空数组同样剔除
		`{"model":"m","input":"hi","tools":[],"tool_choice":"auto"}`,
		// 未携带 tools 同样剔除
		`{"model":"m","input":"hi","tool_choice":"auto"}`,
	} {
		pivot, _, _, err := responsesToPivotRequest([]byte(tc))
		if err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		_ = json.Unmarshal(pivot, &req)
		if _, ok := req["tool_choice"]; ok {
			t.Fatalf("tool_choice must be dropped when no tools: %s → %v", tc, req)
		}
		if _, ok := req["tools"]; ok {
			t.Fatalf("empty tools must be dropped: %s → %v", tc, req)
		}
	}
	// 对照:有 function 工具时 tool_choice 保留
	pivot, _, _, err := responsesToPivotRequest([]byte(`{
		"model":"m", "input":"hi",
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"tool_choice":"auto"}`))
	if err != nil {
		t.Fatal(err)
	}
	var keep map[string]any
	_ = json.Unmarshal(pivot, &keep)
	if keep["tool_choice"] != "auto" {
		t.Fatalf("tool_choice must be kept with tools: %v", keep["tool_choice"])
	}
}

func TestResponsesToPivotInputString(t *testing.T) {
	pivot, _, _, err := responsesToPivotRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(pivot, &req)
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("input string → 1 user message, got %v", msgs)
	}
	if m0, _ := msgs[0].(map[string]any); m0["content"] != "hi" || m0["role"] != "user" {
		t.Fatalf("user message: %v", m0)
	}
}

func TestResponsesToPivotRequestValidation(t *testing.T) {
	if _, _, _, err := responsesToPivotRequest([]byte(`{"input":"x"}`)); err == nil {
		t.Fatal("model required")
	}
	if _, _, _, err := responsesToPivotRequest([]byte(`nope`)); err == nil {
		t.Fatal("invalid json must error")
	}
}

// ---- 响应转换：relayAgg → responses JSON ----

func TestWriteResponsesJSON(t *testing.T) {
	agg := relayAgg{
		Content:          "answer",
		Reasoning:        "think",
		FinishReason:     "tool_calls",
		PromptTokens:     3,
		CompletionTokens: 9,
		ToolCalls: []aggToolCall{
			{ID: "call_1", Name: "get_weather", Arguments: `{"city":"hf"}`},
		},
	}
	w := httptest.NewRecorder()
	writeResponsesJSON(w, "up-model", agg)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body: %s", w.Body.String())
	}
	if resp["object"] != "response" || resp["status"] != "completed" || resp["model"] != "up-model" {
		t.Fatalf("base: %v", resp)
	}
	output, _ := resp["output"].([]any)
	// reasoning + message + function_call = 3
	if len(output) != 3 {
		t.Fatalf("output items: %v", output)
	}
	i0, _ := output[0].(map[string]any)
	if i0["type"] != "reasoning" {
		t.Fatalf("item0: %v", i0)
	}
	i1, _ := output[1].(map[string]any)
	if i1["type"] != "message" || i1["role"] != "assistant" {
		t.Fatalf("item1: %v", i1)
	}
	content, _ := i1["content"].([]any)
	c0, _ := content[0].(map[string]any)
	if c0["type"] != "output_text" || c0["text"] != "answer" {
		t.Fatalf("message content: %v", c0)
	}
	i2, _ := output[2].(map[string]any)
	if i2["type"] != "function_call" || i2["call_id"] != "call_1" || i2["name"] != "get_weather" {
		t.Fatalf("function_call item: %v", i2)
	}
	u, _ := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(3) || u["output_tokens"] != float64(9) || u["total_tokens"] != float64(12) {
		t.Fatalf("usage: %v", u)
	}
}

func TestWriteResponsesJSONIncomplete(t *testing.T) {
	w := httptest.NewRecorder()
	writeResponsesJSON(w, "m", relayAgg{Content: "partial", FinishReason: "length"})
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "incomplete" {
		t.Fatalf("status: %v", resp["status"])
	}
	inc, _ := resp["incomplete_details"].(map[string]any)
	if inc["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details: %v", resp["incomplete_details"])
	}
}

// ---- 上游调用：openai 渠道 + messages/responses 入口（非流式回转）----

func TestUpstreamOpenAIEntryResponsesNonStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "up-model" {
			t.Errorf("model: %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "c1", "object": "chat.completion", "model": "up-model",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "resp answer"},
				"finish_reason": "stop"}],
			"usage": {"prompt_tokens": 4, "completion_tokens": 6}
		}`))
	}))
	defer up.Close()

	pivot, _, _, _ := responsesToPivotRequest([]byte(`{"model":"gpt-5.4","input":"hi"}`))
	upBody := buildUpstreamBody(pivot, "gpt-5.4", "up-model")

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	status, usage, err := h.upstreamCall(t.Context(), h.HTTPClient, up.URL+"/v1/chat/completions", "sk", upBody, false, protoResponses, w)
	if err != nil {
		t.Fatalf("upstreamCall: %v", err)
	}
	if status != 200 || usage.PromptTokens != 4 || usage.CompletionTokens != 6 {
		t.Fatalf("status=%d usage=%+v", status, usage)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body: %s", w.Body.String())
	}
	if resp["object"] != "response" {
		t.Fatalf("object: %v", resp["object"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output: %v", output)
	}
	i0, _ := output[0].(map[string]any)
	if i0["type"] != "message" {
		t.Fatalf("item: %v", i0)
	}
	content, _ := i0["content"].([]any)
	c0, _ := content[0].(map[string]any)
	if c0["text"] != "resp answer" {
		t.Fatalf("text: %v", c0)
	}
}

func TestUpstreamOpenAIEntryMessagesNonStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "c1", "object": "chat.completion", "model": "up-model",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "hey"},
				"finish_reason": "stop"}],
			"usage": {"prompt_tokens": 2, "completion_tokens": 3}
		}`))
	}))
	defer up.Close()

	pivot, _, _, _ := anthropicToPivotRequest([]byte(`{"model":"claude","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`))
	upBody := buildUpstreamBody(pivot, "claude", "up-model")

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	status, usage, err := h.upstreamCall(t.Context(), h.HTTPClient, up.URL, "sk", upBody, false, protoAnthropic, w)
	if err != nil {
		t.Fatalf("upstreamCall: %v", err)
	}
	if status != 200 || usage.PromptTokens != 2 || usage.CompletionTokens != 3 {
		t.Fatalf("status=%d usage=%+v", status, usage)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body: %s", w.Body.String())
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Fatalf("anthropic shape: %v", resp)
	}
	sr, _ := resp["stop_reason"].(string)
	if sr != "end_turn" {
		t.Fatalf("stop_reason: %v", resp["stop_reason"])
	}
	u, _ := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(2) || u["output_tokens"] != float64(3) {
		t.Fatalf("usage: %v", u)
	}
}

// ---- openai JSON → agg ----

func TestOpenaiJSONToAgg(t *testing.T) {
	data := []byte(`{
		"choices": [{
			"message": {"role": "assistant", "content": "ok", "reasoning_content": "why",
				"tool_calls": [{"id": "call_9", "type": "function",
					"function": {"name": "f", "arguments": "{\"a\":1}"}}]},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 2}
	}`)
	agg, ok := openaiJSONToAgg(data)
	if !ok {
		t.Fatal("parse failed")
	}
	if agg.Content != "ok" || agg.Reasoning != "why" || agg.FinishReason != "tool_calls" {
		t.Fatalf("agg: %+v", agg)
	}
	if len(agg.ToolCalls) != 1 || agg.ToolCalls[0].ID != "call_9" || agg.ToolCalls[0].Arguments != `{"a":1}` {
		t.Fatalf("tools: %+v", agg.ToolCalls)
	}
	if agg.PromptTokens != 1 || agg.CompletionTokens != 2 {
		t.Fatalf("usage: %+v", agg)
	}

	// error 形状
	agg2, ok2 := openaiJSONToAgg([]byte(`{"error":{"message":"boom"}}`))
	if !ok2 || agg2.ErrMsg != "boom" {
		t.Fatalf("error agg: %+v ok=%v", agg2, ok2)
	}
}

// ---- codex 上游 + responses 入口透传（含嵌套 usage 记账）----

func TestUpstreamCodexResponsesEntryPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"hey"}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":8}}}`,
			``,
		}, "\n")))
	}))
	defer up.Close()

	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
	h := NewHandler(nil)
	w := httptest.NewRecorder()
	status, usage, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, true, protoResponses, w, nil)
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 8 || !usage.Found {
		t.Fatalf("nested usage extraction: %+v", usage)
	}
	if !strings.Contains(w.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("raw SSE must pass through: %s", w.Body.String())
	}
}

// ---- reasoning effort 归一化：codex "max" 等值不能原样透传给 chat 上游 ----

func TestResponsesToPivotReasoningEffort(t *testing.T) {
	get := func(t *testing.T, effort string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model": "m", "input": "hi",
			"reasoning": map[string]any{"effort": effort, "summary": "auto"},
		})
		pivot, _, _, err := responsesToPivotRequest(body)
		if err != nil {
			t.Fatalf("responsesToPivotRequest(%s): %v", effort, err)
		}
		var out map[string]any
		_ = json.Unmarshal(pivot, &out)
		return out
	}

	// 合法值原样保留
	for _, e := range []string{"low", "medium", "high", "xhigh", "none"} {
		if v, _ := get(t, e)["reasoning_effort"].(string); v != e {
			t.Fatalf("effort %s: got %v", e, v)
		}
	}
	// codex 专有 "max" → "xhigh"（chat 上游不认 max，会 400）
	if v, _ := get(t, "max")["reasoning_effort"].(string); v != "xhigh" {
		t.Fatalf("max should map to xhigh, got %v", v)
	}
	// 未知值 → 丢弃字段（用上游默认值）
	if _, ok := get(t, "ultra")["reasoning_effort"]; ok {
		t.Fatalf("unknown effort should be dropped")
	}
}

// ---- tools 过滤：chat 端点只认 function/custom，其他类型丢弃而非透传 ----

func TestResponsesToPivotToolsFiltering(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[
		{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}},
		{"type":"custom","name":"nota","description":"freeform"},
		{"type":"local_shell"}
	]}`
	pivot, _, _, err := responsesToPivotRequest([]byte(body))
	if err != nil {
		t.Fatalf("responsesToPivotRequest: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(pivot, &out)
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("want 2 tools (local_shell dropped), got %d: %v", len(tools), tools)
	}
	fn, _ := tools[0].(map[string]any)
	if fn["type"] != "function" {
		t.Fatalf("tool[0] type: %v", fn["type"])
	}
	inner, _ := fn["function"].(map[string]any)
	if inner == nil || inner["name"] != "shell" {
		t.Fatalf("function nesting broken: %v", fn)
	}
	cu, _ := tools[1].(map[string]any)
	if cu["type"] != "custom" || cu["name"] != "nota" {
		t.Fatalf("custom tool: %v", cu)
	}
}

// ---- content part 形态：单 part 不能输出裸对象（上游 pydantic 会 400）----

func TestResponsesToPivotSinglePartShape(t *testing.T) {
	// 单文本 part → 退化为纯字符串（chat content 的合法形态之一）
	pivot, _, _, err := responsesToPivotRequest([]byte(
		`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(pivot, &out)
	msgs, _ := out["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if s, ok := m0["content"].(string); !ok || s != "你好" {
		t.Fatalf("single text part should degrade to string, got %T: %v", m0["content"], m0["content"])
	}

	// 多 part → part 数组
	pivot2, _, _, _ := responsesToPivotRequest([]byte(
		`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}]}`))
	_ = json.Unmarshal(pivot2, &out)
	msgs2, _ := out["messages"].([]any)
	m1, _ := msgs2[0].(map[string]any)
	arr, ok := m1["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multi parts should stay array, got %T: %v", m1["content"], m1["content"])
	}
}
