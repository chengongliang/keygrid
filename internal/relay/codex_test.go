package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// ---- Codex 请求转换：chat/completions → responses ----

func TestBuildCodexRequestShape(t *testing.T) {
	chat := `{
		"model": "gpt-5.4",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "system", "content": "ignored second system"},
			{"role": "user", "content": [{"type": "text", "text": "hello"}]},
			{"role": "assistant", "content": null,
			 "tool_calls": [{"id": "call_1", "type": "function",
				"function": {"name": "get_weather", "arguments": "{\"city\":\"hf\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "18C sunny"},
			{"role": "assistant", "content": "It is 18C.", "reasoning_content": "check weather db"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "w",
			"parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}],
		"max_tokens": 256,
		"temperature": 0.3,
		"reasoning_effort": "high"
	}`
	out, err := BuildCodexRequest([]byte(chat), "gpt-5.4")
	if err != nil {
		t.Fatalf("BuildCodexRequest: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if req["model"] != "gpt-5.4" || req["stream"] != true || req["store"] != false {
		t.Fatalf("base fields: model=%v stream=%v store=%v", req["model"], req["stream"], req["store"])
	}
	if req["instructions"] != "be terse" {
		t.Fatalf("instructions = %v (only first system wins)", req["instructions"])
	}
	if _, ok := req["max_output_tokens"]; ok {
		t.Fatalf("max_tokens 系必须丢弃（ChatGPT backend 拒绝 max_output_tokens）")
	}
	if _, ok := req["temperature"]; ok {
		t.Fatalf("Codex 不支持 temperature，最终请求必须删除: %v", req["temperature"])
	}
	r, _ := req["reasoning"].(map[string]any)
	if r == nil || r["effort"] != "high" || r["summary"] != "auto" {
		t.Fatalf("reasoning_effort → reasoning: %v", req["reasoning"])
	}

	input, _ := req["input"].([]any)
	if len(input) != 5 { // user msg + function_call + function_call_output + reasoning + assistant msg
		t.Fatalf("input items = %d: %s", len(input), out)
	}
	userMsg, _ := input[0].(map[string]any)
	if userMsg["role"] != "user" {
		t.Fatalf("item 0 should be user message: %v", userMsg)
	}
	fnCall, _ := input[1].(map[string]any)
	if fnCall["type"] != "function_call" || fnCall["call_id"] != "call_1" || fnCall["name"] != "get_weather" {
		t.Fatalf("tool_calls → function_call item: %v", fnCall)
	}
	if fnCall["arguments"] != `{"city":"hf"}` {
		t.Fatalf("arguments passthrough: %v", fnCall["arguments"])
	}
	fnOut, _ := input[2].(map[string]any)
	if fnOut["type"] != "function_call_output" || fnOut["call_id"] != "call_1" || fnOut["output"] != "18C sunny" {
		t.Fatalf("tool role → function_call_output: %v", fnOut)
	}
	// 最后一条 assistant 的 reasoning_content → reasoning item（紧邻其 message 前）
	reasoning, _ := input[3].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("assistant reasoning_content must become reasoning item: %v", reasoning)
	}
	asstMsg, _ := input[4].(map[string]any)
	if asstMsg["role"] != "assistant" {
		t.Fatalf("item 4 should be assistant message: %v", asstMsg)
	}

	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	flat, _ := tools[0].(map[string]any)
	// responses 工具定义是扁平的（function 字段不嵌套）
	if flat["type"] != "function" || flat["name"] != "get_weather" || flat["description"] != "w" {
		t.Fatalf("tools must be flattened: %v", flat)
	}
	if _, nested := flat["function"]; nested {
		t.Fatalf("responses tool must not nest function: %v", flat)
	}
}

func TestResolveAndInjectCodexSession(t *testing.T) {
	bodyKeys := []string{"prompt_cache_key", "session_id", "conversation_id"}
	for i, key := range bodyKeys {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		r.Header.Set("x-session-id", "header-session")
		payload := map[string]any{}
		for j := i; j < len(bodyKeys); j++ {
			payload[bodyKeys[j]] = bodyKeys[j] + "-value"
		}
		body, _ := json.Marshal(payload)
		if got := resolveCodexSession(r, body, 1, 2, 3); got != key+"-value" {
			t.Fatalf("body priority %s: %q", key, got)
		}
	}

	headerKeys := []string{"x-session-id", "session-id", "session_id", "x-amp-thread-id", "x-client-request-id"}
	for i, key := range headerKeys {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		for j := i; j < len(headerKeys); j++ {
			r.Header.Set(headerKeys[j], headerKeys[j]+"-value")
		}
		if got := resolveCodexSession(r, []byte(`{}`), 1, 2, 3); got != key+"-value" {
			t.Fatalf("header priority %s: %q", key, got)
		}
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	longValue := strings.Repeat("会话", 100)
	longBody, _ := json.Marshal(map[string]any{"prompt_cache_key": longValue})
	bounded := resolveCodexSession(r, longBody, 1, 2, 3)
	if len(bounded) > codexSessionMaxLength || !strings.HasPrefix(bounded, "kg_") || bounded == longValue {
		t.Fatalf("long session must be bounded opaque hash: len=%d %q", len(bounded), bounded)
	}
	if again := resolveCodexSession(r, longBody, 1, 2, 3); again != bounded {
		t.Fatalf("long session hash must be stable: %q %q", bounded, again)
	}

	derived1 := resolveCodexSession(r, []byte(`{}`), 1, 2, 3)
	derived2 := resolveCodexSession(r, []byte(`{}`), 1, 2, 3)
	if derived1 != derived2 || len(derived1) > codexSessionMaxLength || !strings.HasPrefix(derived1, "kg_") || strings.Contains(derived1, "1:2:3") {
		t.Fatalf("derived session must be stable, bounded and opaque: %q %q", derived1, derived2)
	}
	for _, ids := range [][3]int64{{2, 2, 3}, {1, 9, 3}, {1, 2, 4}} {
		if got := resolveCodexSession(r, []byte(`{}`), ids[0], ids[1], ids[2]); got == derived1 {
			t.Fatalf("fallback identities must be isolated: ids=%v got=%q", ids, got)
		}
	}

	injected, err := injectCodexSession([]byte(`{"model":"m","prompt_cache_key":"existing"}`), derived1)
	if err != nil {
		t.Fatal(err)
	}
	if got := codexSessionFromBody(injected); got != "existing" {
		t.Fatalf("existing cache key overwritten: %q", got)
	}
	injected, err = injectCodexSession([]byte(`{"model":"m"}`), derived1)
	if err != nil || codexSessionFromBody(injected) != derived1 {
		t.Fatalf("missing cache key not injected: body=%s err=%v", injected, err)
	}
}

func TestIsCodexProvider(t *testing.T) {
	if !IsCodexProvider(&model.Provider{Kind: "oauth", OAuthProvider: "openai"}) {
		t.Fatal("oauth/openai must be codex")
	}
	if IsCodexProvider(&model.Provider{Kind: "oauth", OAuthProvider: "anthropic"}) {
		t.Fatal("anthropic is not codex")
	}
	if IsCodexProvider(&model.Provider{Kind: "api_key", OAuthProvider: "openai"}) {
		t.Fatal("api_key channel is not codex transport")
	}
}

// ---- 非流式：responses SSE 聚合为 chat.completion JSON ----

func TestUpstreamCallCodexNonStream(t *testing.T) {
	var gotAuth, gotAcct, gotOriginator, gotUA, gotSession string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAcct = r.Header.Get("chatgpt-account-id")
		gotOriginator = r.Header.Get("originator")
		gotUA = r.Header.Get("User-Agent")
		gotSession = r.Header.Get("session_id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"你好"}`,
			``,
			`data: {"type":"response.reasoning_summary_text.delta","delta":"思考中"}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":34}}}`,
			``,
		}, "\n")))
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
	body, err := injectCodexSession(body, "cache-session")
	if err != nil {
		t.Fatalf("inject session: %v", err)
	}
	status, usage, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok-1", "acct-9", body, false, protoOpenAI, w, nil)
	if err != nil {
		t.Fatalf("upstreamCallCodex: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "Bearer tok-1" || gotAcct != "acct-9" || gotOriginator != "codex_cli_rs" || gotUA != "codex_cli_rs/0.154.0" || gotSession != "cache-session" {
		t.Fatalf("headers: auth=%q acct=%q originator=%q ua=%q session=%q", gotAuth, gotAcct, gotOriginator, gotUA, gotSession)
	}
	if gotBody["stream"] != true || gotBody["store"] != false {
		t.Fatalf("upstream body must force stream+store=false: %v", gotBody)
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 34 || !usage.Found {
		t.Fatalf("usage: %+v", usage)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body: %s", w.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Fatalf("object = %v", resp["object"])
	}
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	if msg["content"] != "你好" || msg["reasoning_content"] != "思考中" {
		t.Fatalf("aggregated message: %v", msg)
	}
	u, _ := resp["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(12) || u["completion_tokens"] != float64(34) {
		t.Fatalf("client usage: %v", u)
	}
}

// ---- 流式：responses SSE → chat.completions chunks ----

func TestUpstreamCallCodexStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"he"}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"llo"}`,
			``,
			`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather"}}`,
			``,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"c"}`,
			``,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"ity\":\"hf\"}"}`,
			``,
			// 真实 Codex 上游：done 事件也带完整 arguments —— 不得与 deltas 重复拼接
			`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"hf\"}"}}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":7}}}`,
			``,
		}, "\n")))
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
	status, usage, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok-1", "", body, true, protoOpenAI, w, nil)
	if err != nil {
		t.Fatalf("upstreamCallCodex: %v", err)
	}
	if status != 200 || usage.PromptTokens != 5 || usage.CompletionTokens != 7 {
		t.Fatalf("status/usage: %d %+v", status, usage)
	}

	// 解析客户端收到的 SSE chunks
	lines := strings.Split(w.Body.String(), "\n")
	var chunks []map[string]any
	var doneSeen bool
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(ln, "data:"))
		if payload == "[DONE]" {
			doneSeen = true
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(payload), &c); err == nil {
			chunks = append(chunks, c)
		}
	}
	if !doneSeen {
		t.Fatal("stream must end with data: [DONE]")
	}

	var content strings.Builder
	var args strings.Builder
	var toolName string
	var finish any
	for i, c := range chunks {
		if c["object"] != "chat.completion.chunk" {
			t.Fatalf("chunk %d object = %v", i, c["object"])
		}
		chs, _ := c["choices"].([]any)
		ch, _ := chs[0].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		if delta != nil {
			if s, ok := delta["content"].(string); ok {
				content.WriteString(s)
				if _, hasRole := delta["role"]; hasRole && content.Len() > len(s) {
					t.Fatal("role must only appear on first delta")
				}
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, _ := tci.(map[string]any)
					fn, _ := tc["function"].(map[string]any)
					if n, ok := fn["name"].(string); ok && n != "" {
						toolName = n
					}
					if a, ok := fn["arguments"].(string); ok {
						args.WriteString(a)
					}
				}
			}
		}
		if ch["finish_reason"] != nil {
			finish = ch["finish_reason"]
		}
	}
	if content.String() != "hello" {
		t.Fatalf("streamed content = %q", content.String())
	}
	if toolName != "get_weather" {
		t.Fatalf("tool name = %q", toolName)
	}
	if args.String() != `{"city":"hf"}` {
		t.Fatalf("streamed args = %q", args.String())
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %v (tool call present)", finish)
	}
}

// ---- 非流式聚合：tool_calls 必须带 name/arguments（OpenAI SDK 可直接执行）----

func TestCodexCustomToolCallOutputShapes(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_custom","name":"apply_patch"}}`,
		``,
		`data: {"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","delta":"line 1\n\"quoted\""}`,
		``,
		`data: {"type":"response.output_item.done","item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_custom","name":"apply_patch","input":"line 1\n\"quoted\""}}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
		``,
	}, "\n")
	agg, err := AggregateCodexStream(strings.NewReader(sse))
	if err != nil || len(agg.ToolCalls) != 1 || agg.ToolCalls[0].Type != "custom_tool_call" {
		t.Fatalf("aggregate custom: %+v err=%v", agg, err)
	}

	chatW := httptest.NewRecorder()
	writeChatCompletionJSON(chatW, "m", *agg)
	var chat map[string]any
	_ = json.Unmarshal(chatW.Body.Bytes(), &chat)
	choices := chat["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	arguments := calls[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	var args map[string]string
	if json.Unmarshal([]byte(arguments), &args) != nil || args["input"] != "line 1\n\"quoted\"" {
		t.Fatalf("chat custom arguments must be valid JSON: %q", arguments)
	}

	responsesW := httptest.NewRecorder()
	writeResponsesJSON(responsesW, "m", *agg)
	var responses map[string]any
	_ = json.Unmarshal(responsesW.Body.Bytes(), &responses)
	output := responses["output"].([]any)
	custom := output[0].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["input"] != "line 1\n\"quoted\"" {
		t.Fatalf("responses custom shape: %v", custom)
	}
}

func TestConvertCodexCustomToolCallStreamArgumentsJSON(t *testing.T) {
	state := newCodexChunkState()
	added := map[string]any{"item": map[string]any{
		"id": "ctc_1", "type": "custom_tool_call", "call_id": "call_custom", "name": "apply_patch",
	}}
	chunk, _, _ := convertCodexEvent(state, "response.output_item.added", added)
	var arguments strings.Builder
	appendArgs := func(c map[string]any) {
		if c == nil {
			return
		}
		choices := c["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		if len(calls) == 0 {
			return
		}
		fn := calls[0].(map[string]any)["function"].(map[string]any)
		arguments.WriteString(str(fn["arguments"]))
	}
	appendArgs(chunk)
	original := "line 1\n\"quoted\"\\path\\end"
	for _, part := range []string{"line 1\n\"", "quoted\"\\", "path\\end"} {
		chunk, _, _ = convertCodexEvent(state, "response.custom_tool_call_input.delta", map[string]any{
			"item_id": "ctc_1", "delta": part,
		})
		appendArgs(chunk)
	}
	chunk, _, _ = convertCodexEvent(state, "response.output_item.done", map[string]any{
		"item_id": "ctc_1", "item": map[string]any{
			"id": "ctc_1", "type": "custom_tool_call", "call_id": "call_custom", "name": "apply_patch", "input": original,
		},
	})
	appendArgs(chunk)
	var decoded map[string]string
	if err := json.Unmarshal([]byte(arguments.String()), &decoded); err != nil || decoded["input"] != original {
		t.Fatalf("stream custom arguments invalid: %q decoded=%q err=%v", arguments.String(), decoded["input"], err)
	}
}

func TestConvertCodexInterleavedToolCallIndexes(t *testing.T) {
	state := newCodexChunkState()
	type seenCall struct {
		index int
		args  string
	}
	seen := make([]seenCall, 0)
	capture := func(chunk map[string]any) {
		if chunk == nil {
			return
		}
		choices := chunk["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			call := raw.(map[string]any)
			fn := call["function"].(map[string]any)
			seen = append(seen, seenCall{index: int(call["index"].(int)), args: str(fn["arguments"])})
		}
	}

	chunk, _, _ := convertCodexEvent(state, "response.output_item.added", map[string]any{"item": map[string]any{
		"id": "ctc_1", "type": "custom_tool_call", "call_id": "c1", "name": "patch",
	}})
	capture(chunk)
	chunk, _, _ = convertCodexEvent(state, "response.output_item.added", map[string]any{"item": map[string]any{
		"id": "fc_2", "type": "function_call", "call_id": "c2", "name": "lookup",
	}})
	capture(chunk)
	chunk, _, _ = convertCodexEvent(state, "response.function_call_arguments.delta", map[string]any{"item_id": "fc_2", "delta": `{"id":`})
	capture(chunk)
	chunk, _, _ = convertCodexEvent(state, "response.custom_tool_call_input.delta", map[string]any{"item_id": "ctc_1", "delta": "a\\b"})
	capture(chunk)
	chunk, _, _ = convertCodexEvent(state, "response.output_item.done", map[string]any{"item": map[string]any{
		"id": "fc_2", "type": "function_call", "call_id": "c2", "name": "lookup", "arguments": `{"id":1}`,
	}})
	capture(chunk)
	chunk, _, _ = convertCodexEvent(state, "response.function_call_arguments.delta", map[string]any{"item_id": "fc_2", "delta": `1}`})
	capture(chunk)
	chunk, _, _ = convertCodexEvent(state, "response.output_item.done", map[string]any{"item": map[string]any{
		"id": "ctc_1", "type": "custom_tool_call", "call_id": "c1", "name": "patch", "input": "a\\b",
	}})
	capture(chunk)

	var custom, function strings.Builder
	for _, call := range seen {
		switch call.index {
		case 0:
			custom.WriteString(call.args)
		case 1:
			function.WriteString(call.args)
		default:
			t.Fatalf("unexpected tool index: %+v", call)
		}
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(custom.String()), &decoded); err != nil || decoded["input"] != "a\\b" {
		t.Fatalf("custom index/JSON broken: %q decoded=%v err=%v", custom.String(), decoded, err)
	}
	if function.String() != `{"id":1}` {
		t.Fatalf("function interleaved args/index broken: %q", function.String())
	}
}

func TestUpstreamCallCodexNonStreamToolCalls(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
			``,
			`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather"}}`,
			``,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"city\":"}`,
			``,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"hf\"}"}`,
			``,
			`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"hf\"}"}}`,
			``,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":9,"output_tokens":3}}}`,
			``,
		}, "\n")))
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`), "gpt-5.4")
	status, usage, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, false, protoOpenAI, w, nil)
	if err != nil || status != 200 {
		t.Fatalf("call: status=%d err=%v", status, err)
	}
	if usage.PromptTokens != 9 || usage.CompletionTokens != 3 {
		t.Fatalf("usage: %+v", usage)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client body: %s", w.Body.String())
	}
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("non-stream must emit executable tool_calls: %v", msg)
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Fatalf("tool call shape: %v", tc)
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"hf"}` {
		t.Fatalf("tool call function: %v", fn)
	}
	if c0["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason: %v", c0["finish_reason"])
	}
}

// cappedReadReader 强制底层流按小切片返回，覆盖 SSE event 跨 Read 的情况。
type cappedReadReader struct {
	r io.Reader
	n int
}

func (r *cappedReadReader) Read(p []byte) (int, error) {
	if len(p) > r.n {
		p = p[:r.n]
	}
	return r.r.Read(p)
}

func TestPeekCodexSSEErrorReplayBoundaries(t *testing.T) {
	tests := []struct {
		name string
		sse  string
		step int
	}{
		{
			name: "event and reads fragmented",
			sse:  "event: response.created\ndata: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n",
			step: 3,
		},
		{
			name: "exact peek limit",
			sse:  strings.Repeat(":", codexSSEPeekLimit),
			step: 257,
		},
		{
			name: "EOF without blank line",
			sse:  `data: {"type":"response.output_text.delta","delta":"tail"}`,
			step: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := io.NopCloser(&cappedReadReader{r: strings.NewReader(tt.sse), n: tt.step})
			resp := &http.Response{StatusCode: http.StatusOK, Body: original}
			status, err := peekCodexSSEError(resp)
			if err != nil || status != 0 {
				t.Fatalf("peek status=%d err=%v", status, err)
			}
			got, readErr := io.ReadAll(resp.Body)
			if readErr != nil || string(got) != tt.sse {
				t.Fatalf("replay lost/duplicated bytes: len(got)=%d len(want)=%d err=%v", len(got), len(tt.sse), readErr)
			}
		})
	}
}

func TestPeekCodexSSEOutputTextContainingErrorPatternWins(t *testing.T) {
	sse := `data: {"type":"response.output_text.delta","delta":"server_is_overloaded is an error code"}` + "\n\n" +
		`data: {"type":"error","error":{"message":"model_at_capacity"}}` + "\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse))}
	status, err := peekCodexSSEError(resp)
	if err != nil || status != 0 {
		t.Fatalf("normal text must commit stream before pattern matching: status=%d err=%v", status, err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != sse {
		t.Fatalf("output+later error replay changed: %q", got)
	}
}

func TestUpstreamCallCodexSSEPreOutputError(t *testing.T) {
	tests := []struct {
		name string
		sse  string
		want string
	}{
		{
			name: "reasoning 后 response.failed capacity",
			sse: strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
				``,
				`data: {"type":"response.reasoning_summary_text.delta","delta":"checking"}`,
				``,
				`event: response.failed`,
				`data: {"type":"response.failed","response":{"error":{"message":"Selected model is at capacity. Please try a different model."}}}`,
				``,
			}, "\n"),
			want: "Selected model is at capacity",
		},
		{
			name: "error 事件 overloaded",
			sse: "event: error\n" +
				`data: {"type":"error","error":{"message":"server_is_overloaded"}}` + "\n\n",
			want: "server_is_overloaded",
		},
		{
			name: "普通事件中的暂态文本",
			sse:  `data: {"type":"response.in_progress","detail":"service_unavailable_error"}` + "\n\n",
			want: "service_unavailable_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.sse)
			}))
			defer up.Close()

			h := NewHandler(nil)
			w := httptest.NewRecorder()
			body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
			status, _, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, true, protoOpenAI, w, nil)
			if status != http.StatusServiceUnavailable || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("status=%d err=%v, want 503 containing %q", status, err, tt.want)
			}
			if w.Body.Len() != 0 {
				t.Fatalf("前置错误不可写客户端，got %q", w.Body.String())
			}
		})
	}
}

func TestUpstreamCallCodexSSEErrorAfterOutputStaysInStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"started"}`,
		``,
		`data: {"type":"error","error":{"message":"model_at_capacity"}}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
	status, _, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, true, protoOpenAI, w, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("正常输出后的错误不可 failover: status=%d err=%v", status, err)
	}
	got := w.Body.String()
	if !strings.Contains(got, "started") || !strings.Contains(got, "[Error] model_at_capacity") {
		t.Fatalf("应维持当前流内错误行为: %s", got)
	}
}

func TestUpstreamCallCodexSSEPeekReplaysResponsesBytes(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"why"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":3}}}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequestFromResponses([]byte(`{"model":"m","input":"hi","stream":true}`), "gpt-5.4")
	status, usage, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, true, protoResponses, w, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if got := w.Body.String(); got != sse {
		t.Fatalf("探测回放改变了 SSE\ngot:  %q\nwant: %q", got, sse)
	}
	if usage.PromptTokens != 2 || usage.CompletionTokens != 3 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestUpstreamCallCodexSSEPeekReplaysToolStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
		``,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup"}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"id\":1}"}`,
		``,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"}}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
	status, _, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, true, protoOpenAI, w, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	got := w.Body.String()
	if !strings.Contains(got, `"name":"lookup"`) || !strings.Contains(got, `{\"id\":1}`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("工具流探测后内容不完整: %s", got)
	}
}

// ---- 上游 4xx：不写响应体，可 failover ----

func TestUpstreamCallCodexUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"token expired"}`))
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
	status, _, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, false, protoOpenAI, w, nil)
	if status != 401 || err == nil {
		t.Fatalf("expected 401 + error, got %d %v", status, err)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("nothing should be written to client on upstream error, got %q", w.Body.String())
	}
}

// ---- Codex base_url 归一化（历史默认值缺 /codex/responses 尾段）----

func TestNormalizeCodexBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// 本次故障场景：历史默认值缺尾段 → 补全
		{"https://chatgpt.com/backend-api", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/codex", "https://chatgpt.com/backend-api/codex/responses"},
		{"  https://chatgpt.com/backend-api/codex/responses/  ", "https://chatgpt.com/backend-api/codex/responses"},
		// 完整 endpoint / 自定义第三方 Responses endpoint 原样
		{"https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://relay.example.com/v1/responses", "https://relay.example.com/v1/responses"},
		{"https://my-worker.example.workers.dev/backend-api", "https://my-worker.example.workers.dev/backend-api/codex/responses"},
		// 非官方形态不猜（非 Codex 兼容地址原样透出，由上游报错）
		{"https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeCodexBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeCodexBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
		// 幂等：归一化结果再归一化必须稳定（转发路径每次请求都会调用）
		if got := NormalizeCodexBaseURL(c.want); got != c.want {
			t.Errorf("NormalizeCodexBaseURL(%q) 非幂等: %q", c.want, got)
		}
	}
}

// ---- 200 + 非 SSE 正文：必须明确报错（可 failover），不能当 SSE 透传 ----

func TestPeekCodexSSENonSSEBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		ct   string
	}{
		{"html", "<!DOCTYPE html><html><head><title>404</title></head><body>not found</body></html>", "text/html; charset=utf-8"},
		{"json", `{"detail":"Not Found"}`, "application/json"},
		{"plain text", "Bad Gateway", "text/plain"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{tt.ct}},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}
			status, err := peekCodexSSEError(resp)
			if err == nil || status != http.StatusBadGateway {
				t.Fatalf("非 SSE 正文必须快速失败: status=%d err=%v", status, err)
			}
			var ne *codexNonSSEError
			if !errors.As(err, &ne) {
				t.Fatalf("error type = %T, want *codexNonSSEError", err)
			}
			if ne.Detail() == "" || len([]rune(ne.Detail())) > 200 {
				t.Fatalf("detail 必须非空且截断: %q", ne.Detail())
			}
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "<html") {
				t.Fatalf("Error() 不得携带上游正文（可能回显请求）: %q", err.Error())
			}
		})
	}
}

// 非 SSE 判定不能误伤正常事件流（含注释心跳、多字段事件）
func TestCodexSSEFrameLike(t *testing.T) {
	frames := []string{
		"data: {\"type\":\"response.created\"}\n\n",
		"event: response.created\ndata: {}\n\n",
		": keep-alive\n\n",
		"id: 7\nretry: 100\ndata: {}\n\n",
		"DATA: {\"type\":\"response.created\"}\n\n", // 字段名大小写不敏感
		"\n", // 纯空行：无判定证据
	}
	for _, s := range frames {
		if !codexSSEFrameLike([]byte(s)) {
			t.Errorf("must be accepted as SSE frame: %q", s)
		}
	}
	nonFrames := []string{
		`{"detail":"Not Found"}`,
		"<!DOCTYPE html>\n<html>",
		"<html><head><title>403 Forbidden</title></head>",
		"Bad Gateway",
		"{\"error\":{\"message\":\"x\"}}",
	}
	for _, s := range nonFrames {
		if codexSSEFrameLike([]byte(s)) {
			t.Errorf("must be rejected as non-SSE: %q", s)
		}
	}
}

// 200 非 SSE 时上游正文不得写进客户端响应（failover 前置条件）
func TestUpstreamCallCodexNonSSEWritesNothing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>upstream error page</body></html>"))
	}))
	defer up.Close()

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, _ := BuildCodexRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "gpt-5.4")
	status, _, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, up.URL, "tok", "", body, true, protoResponses, w, nil)
	if err == nil || status != http.StatusBadGateway {
		t.Fatalf("status=%d err=%v, want 502", status, err)
	}
	if w.Body.Len() != 0 || w.Code != http.StatusOK {
		t.Fatalf("非 SSE 上游不得写客户端: code=%d body=%q", w.Code, w.Body.String())
	}
}

// 存量错渠道（历史默认值缺 /codex/responses 尾段）经归一化后真实打到完整
// endpoint，且 responses 入口 SSE 透传、usage 记账均正常（relay 转发侧接线验证）。
func TestUpstreamCallCodexNormalizedHistoricalBaseURL(t *testing.T) {
	var gotPath string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.5\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"output\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":5}}}\n\n"))
	}))
	defer up.Close()

	// 上游地址 = 历史错误默认值形态（只有 /backend-api）
	target := NormalizeCodexBaseURL(up.URL + "/backend-api")
	if !strings.HasSuffix(target, "/backend-api/codex/responses") {
		t.Fatalf("normalized target = %q", target)
	}

	h := NewHandler(nil)
	w := httptest.NewRecorder()
	body, err := BuildCodexRequestFromResponses([]byte(`{"model":"gpt-5.5","input":"hi","stream":true}`), "gpt-5.5")
	if err != nil {
		t.Fatalf("BuildCodexRequestFromResponses: %v", err)
	}
	status, usage, err := h.upstreamCallCodex(t.Context(), h.HTTPClient, target, "tok", "", body, true, protoResponses, w, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if gotPath != "/backend-api/codex/responses" {
		t.Fatalf("上游路径 = %q, want /backend-api/codex/responses", gotPath)
	}
	if !strings.Contains(string(gotBody), `"stream":true`) {
		t.Fatalf("上游请求体: %s", gotBody)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 5 || !usage.Found {
		t.Fatalf("usage = %+v, want 11/5", usage)
	}
	if got := w.Body.String(); !strings.Contains(got, "response.completed") {
		t.Fatalf("客户端未收到完整 SSE: %s", got)
	}
}
