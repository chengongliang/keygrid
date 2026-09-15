package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- anthropicInState：anthropic SSE → chat chunks ----

func TestAnthropicInStateTextAndUsage(t *testing.T) {
	in := newAnthropicInState("up-model")
	var chunks []map[string]any
	onChunk := func(c map[string]any) error {
		// 经 marshal→unmarshal 模拟真实线上传输（指针/int 归一为 JSON 类型）
		b, _ := json.Marshal(c)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		chunks = append(chunks, m)
		return nil
	}

	evs := []string{
		`{"type":"message_start","message":{"model":"up-model","usage":{"input_tokens":10}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"he"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"llo"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		`{"type":"message_stop"}`,
	}
	for _, e := range evs {
		var ev map[string]any
		if err := json.Unmarshal([]byte(e), &ev); err != nil {
			t.Fatal(err)
		}
		tn, _ := ev["type"].(string)
		if err := in.feedAnthropicEvent(onChunk, tn, ev); err != nil {
			t.Fatal(err)
		}
	}

	// role chunk + 2×content + thinking + finish = 5
	if len(chunks) != 5 {
		t.Fatalf("chunks: %d → %v", len(chunks), chunks)
	}
	c0 := chunks[0]["choices"].([]any)[0].(map[string]any)
	if d, _ := c0["delta"].(map[string]any); d["role"] != "assistant" {
		t.Fatalf("first chunk must carry role: %v", chunks[0])
	}
	c1 := chunks[1]["choices"].([]any)[0].(map[string]any)
	d1, _ := c1["delta"].(map[string]any)
	if d1["content"] != "he" {
		t.Fatalf("content delta: %v", d1)
	}
	c3 := chunks[3]["choices"].([]any)[0].(map[string]any)
	d3, _ := c3["delta"].(map[string]any)
	if d3["reasoning_content"] != "hmm" {
		t.Fatalf("thinking delta: %v", d3)
	}
	// finish chunk
	c4 := chunks[4]["choices"].([]any)[0].(map[string]any)
	if c4["finish_reason"] != "stop" {
		t.Fatalf("finish: %v", c4["finish_reason"])
	}
	u := in.usage()
	if u.PromptTokens != 10 || u.CompletionTokens != 4 || !u.Found {
		t.Fatalf("usage: %+v", u)
	}
}

func TestAnthropicInStateToolUse(t *testing.T) {
	in := newAnthropicInState("m")
	var chunks []map[string]any
	onChunk := func(c map[string]any) error {
		b, _ := json.Marshal(c)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		chunks = append(chunks, m)
		return nil
	}
	evs := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":2}}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ty\":\"hf\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`,
		`{"type":"message_stop"}`,
	}
	for _, e := range evs {
		var ev map[string]any
		_ = json.Unmarshal([]byte(e), &ev)
		tn, _ := ev["type"].(string)
		if err := in.feedAnthropicEvent(onChunk, tn, ev); err != nil {
			t.Fatal(err)
		}
	}

	// role + tool start + 2×args + finish = 5
	if len(chunks) != 5 {
		t.Fatalf("chunks: %v", chunks)
	}
	c1 := chunks[1]["choices"].([]any)[0].(map[string]any)
	d1, _ := c1["delta"].(map[string]any)
	tcs, _ := d1["tool_calls"].([]any)
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["index"] != float64(0) || fn["name"] != "get_weather" {
		t.Fatalf("tool start chunk: %v", tc)
	}
	// finish_reason=tool_calls
	c4 := chunks[4]["choices"].([]any)[0].(map[string]any)
	if c4["finish_reason"] != "tool_calls" {
		t.Fatalf("finish: %v", c4["finish_reason"])
	}
}

// ---- anthropicOutState：chat chunks → anthropic SSE 事件 ----

func parseAnthropicEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
			out = append(out, ev)
		}
	}
	return out
}

func TestAnthropicOutStateSequence(t *testing.T) {
	st := newAnthropicOutState("up-model")
	w := httptest.NewRecorder()

	chunks := []string{
		`{"model":"up-model","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
	}
	for _, c := range chunks {
		var m map[string]any
		_ = json.Unmarshal([]byte(c), &m)
		if _, _, err := st.feed(w.Body, m); err != nil {
			t.Fatal(err)
		}
	}

	evs := parseAnthropicEvents(t, w.Body.String())
	types := make([]string, 0, len(evs))
	for _, e := range evs {
		tn, _ := e["type"].(string)
		types = append(types, tn)
	}
	want := []string{
		"message_start",
		"content_block_start", // text
		"content_block_delta", // he
		"content_block_delta", // llo
		"content_block_stop",  // text block 关闭
		"content_block_start", // tool_use
		"content_block_delta", // input_json_delta
		"content_block_stop",  // tool block 关闭
		"message_delta",
		"message_stop",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\ngot  %v\nwant %v", types, want)
	}
	// message_delta 携带 stop_reason + usage
	md := evs[len(evs)-2]
	delta, _ := md["delta"].(map[string]any)
	if delta["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason: %v", delta)
	}
	u, _ := md["usage"].(map[string]any)
	if u["input_tokens"] != float64(5) || u["output_tokens"] != float64(7) {
		t.Fatalf("message_delta usage: %v", u)
	}
}

// ---- responsesOutState：chat chunks → responses events ----

func TestResponsesOutStateSequence(t *testing.T) {
	st := newResponsesOutState("up-model")
	w := httptest.NewRecorder()

	chunks := []string{
		`{"model":"up-model","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`,
	}
	for _, c := range chunks {
		var m map[string]any
		_ = json.Unmarshal([]byte(c), &m)
		if _, _, err := st.feed(w.Body, m); err != nil {
			t.Fatal(err)
		}
	}

	var types []string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
			tn, _ := ev["type"].(string)
			types = append(types, tn)
		}
	}
	want := []string{
		"response.created",
		"response.output_item.added",  // message item
		"response.content_part.added", // output_text part
		"response.output_text.delta",  // he
		"response.output_text.delta",  // llo
		"response.output_item.added",  // function_call item
		"response.function_call_arguments.delta",
		"response.output_text.done",  // text done
		"response.content_part.done", // part done
		"response.output_item.done",  // message done
		"response.function_call_arguments.done",
		"response.output_item.done", // function_call done
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\ngot  %v\nwant %v", types, want)
	}
}

func TestResponsesOutStatePreservesItemIndexes(t *testing.T) {
	st := newResponsesOutState("up-model")
	w := httptest.NewRecorder()
	chunks := []string{
		`{"choices":[{"delta":{"content":"text","reasoning_content":"think","tool_calls":[{"index":0,"id":"call_a","function":{"name":"a","arguments":"{}"}},{"index":1,"id":"call_b","function":{"name":"b","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, c := range chunks {
		var m map[string]any
		_ = json.Unmarshal([]byte(c), &m)
		if _, _, err := st.feed(w.Body, m); err != nil {
			t.Fatal(err)
		}
	}

	indexes := map[string]float64{}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil || ev["type"] != "response.output_item.added" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		indexes[item["id"].(string)] = ev["output_index"].(float64)
	}
	if len(indexes) != 4 {
		t.Fatalf("output item indexes: %v", indexes)
	}
	seen := map[float64]bool{}
	for _, index := range indexes {
		if seen[index] {
			t.Fatalf("output index reused: %v", indexes)
		}
		seen[index] = true
	}
}

// ---- 桥端到端：openai 上游流 + messages 入口 ----

func TestBridgeChatChunksToMessagesE2E(t *testing.T) {
	upSSE := strings.Join([]string{
		`data: {"model":"up-model","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`,
		`data: {"model":"up-model","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`data: {"model":"up-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upSSE))
	}))
	defer up.Close()

	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	status, usage, err := bridgeChatChunksToEntry(w, resp, protoAnthropic, "up-model")
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Fatalf("usage: %+v", usage)
	}
	evs := parseAnthropicEvents(t, w.Body.String())
	if len(evs) == 0 {
		t.Fatalf("no events: %s", w.Body.String())
	}
	last := evs[len(evs)-1]
	if last["type"] != "message_stop" {
		t.Fatalf("must end with message_stop: %v", evs)
	}
	// 文本完整
	var text strings.Builder
	for _, e := range evs {
		if e["type"] == "content_block_delta" {
			if d, ok := e["delta"].(map[string]any); ok {
				if s, ok := d["text"].(string); ok {
					text.WriteString(s)
				}
			}
		}
	}
	if text.String() != "hello" {
		t.Fatalf("assembled text: %q", text.String())
	}
}

// ---- 桥端到端：anthropic 上游流 + chat 入口 ----

func TestBridgeAnthropicToChatE2E(t *testing.T) {
	upSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"up-model","usage":{"input_tokens":6}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bonjour"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upSSE))
	}))
	defer up.Close()

	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	status, usage, err := bridgeAnthropicSSEToEntry(w, resp, protoOpenAI, "up-model")
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if usage.PromptTokens != 6 || usage.CompletionTokens != 2 {
		t.Fatalf("usage: %+v", usage)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"bonjour"`) {
		t.Fatalf("chat chunk content: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("must end with [DONE]: %s", body)
	}
}

// ---- 桥端到端：anthropic 上游流 + responses 入口 ----

func TestBridgeAnthropicToResponsesE2E(t *testing.T) {
	upSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"up-model","usage":{"input_tokens":6}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"salut"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upSSE))
	}))
	defer up.Close()

	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	status, usage, err := bridgeAnthropicSSEToEntry(w, resp, protoResponses, "up-model")
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if usage.PromptTokens != 6 || usage.CompletionTokens != 2 {
		t.Fatalf("usage: %+v", usage)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"response.output_text.delta"`) ||
		!strings.Contains(body, `"delta":"salut"`) {
		t.Fatalf("responses events: %s", body)
	}
	if !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("must end with response.completed: %s", body)
	}
}

// ---- 桥端到端：codex(responses) 上游流 + messages 入口 ----

func TestBridgeCodexToMessagesE2E(t *testing.T) {
	upSSE := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"salut"}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":4,"output_tokens":1}}}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upSSE))
	}))
	defer up.Close()

	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	status, usage, err := bridgeCodexSSEToMessages(w, resp, "gpt-5.4")
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if usage.PromptTokens != 4 || usage.CompletionTokens != 1 {
		t.Fatalf("usage: %+v", usage)
	}
	evs := parseAnthropicEvents(t, w.Body.String())
	if len(evs) == 0 || evs[0]["type"] != "message_start" {
		t.Fatalf("must start with message_start: %s", w.Body.String())
	}
	last := evs[len(evs)-1]
	if last["type"] != "message_stop" {
		t.Fatalf("must end with message_stop: %v", evs)
	}
	// 文本从 responses delta 桥到 anthropic text_delta
	var text strings.Builder
	for _, e := range evs {
		if e["type"] == "content_block_delta" {
			if d, ok := e["delta"].(map[string]any); ok {
				if s, ok := d["text"].(string); ok {
					text.WriteString(s)
				}
			}
		}
	}
	if text.String() != "salut" {
		t.Fatalf("assembled text: %q", text.String())
	}
}

// ---- openai 上游流 + responses 入口（桥端到端）----

func TestBridgeChatChunksToResponsesE2E(t *testing.T) {
	upSSE := strings.Join([]string{
		`data: {"model":"up-model","choices":[{"index":0,"delta":{"role":"assistant","content":"yo"}}]}`,
		`data: {"model":"up-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upSSE))
	}))
	defer up.Close()

	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	status, usage, err := bridgeChatChunksToEntry(w, resp, protoResponses, "up-model")
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if usage.PromptTokens != 1 || usage.CompletionTokens != 1 {
		t.Fatalf("usage: %+v", usage)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"response.output_text.delta"`) ||
		!strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("responses events: %s", body)
	}
}

// ---- usage 提取兼容性 ----

func TestExtractChunkUsageCompat(t *testing.T) {
	// anthropic message_start（message.usage 嵌套）
	u, ok := extractChunkUsage(json.RawMessage(
		`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":5}}}`))
	if !ok || u.PromptTokens != 5 {
		t.Fatalf("anthropic start: %+v ok=%v", u, ok)
	}
	// anthropic message_delta（顶层 usage 仅 output_tokens）
	u, ok = extractChunkUsage(json.RawMessage(
		`{"type":"message_delta","usage":{"output_tokens":8}}`))
	if !ok || u.CompletionTokens != 8 {
		t.Fatalf("anthropic delta: %+v ok=%v", u, ok)
	}
	// responses completed（response.usage 嵌套）
	u, ok = extractChunkUsage(json.RawMessage(
		`{"type":"response.completed","response":{"model":"m","usage":{"input_tokens":2,"output_tokens":3}}}`))
	if !ok || u.PromptTokens != 2 || u.CompletionTokens != 3 || u.Model != "m" {
		t.Fatalf("responses completed: %+v ok=%v", u, ok)
	}
	// openai chunk（原形状不受影响）
	u, ok = extractChunkUsage(json.RawMessage(
		`{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	if !ok || u.PromptTokens != 1 || u.CompletionTokens != 2 {
		t.Fatalf("openai: %+v ok=%v", u, ok)
	}
	// 无 usage
	if _, ok = extractChunkUsage(json.RawMessage(`{"choices":[]}`)); ok {
		t.Fatal("no usage must not be found")
	}
}
