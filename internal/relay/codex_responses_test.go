package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- Codex 渠道：responses 入口直转（避免 pivot 双重有损转换）----

func TestBuildCodexRequestFromResponses(t *testing.T) {
	// Codex CLI 真实请求形状：local_shell/custom 工具、input 内嵌 reasoning 与
	// function_call item、reasoning.effort、include encrypted_content
	raw := `{
		"model": "gpt-5.6-luna",
		"instructions": "You are Codex",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run git diff"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}],
			 "encrypted_content":"gAAAA"},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"file list"}
		],
		"tools": [
			{"type":"local_shell"},
			{"type":"custom","name":"apply_patch"},
			{"type":"function","name":"view_image","description":"d","parameters":{"type":"object"}},
			{"type":"web_search"}
		],
		"tool_choice": "auto",
		"parallel_tool_calls": false,
		"reasoning": {"effort": "max", "summary": "auto"},
		"include": ["reasoning.encrypted_content"],
		"temperature": 0.5,
		"store": true,
		"previous_response_id": "resp_123",
		"prompt_cache_key": "k-1"
	}`
	out, err := BuildCodexRequestFromResponses([]byte(raw), "gpt-5.6-luna-up")
	if err != nil {
		t.Fatalf("BuildCodexRequestFromResponses: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// model_map 反查后的上游名 + 强制流式/非持久化
	if req["model"] != "gpt-5.6-luna-up" {
		t.Fatalf("model = %v", req["model"])
	}
	if req["stream"] != true || req["store"] != false {
		t.Fatalf("must force stream:true store:false, got stream=%v store=%v", req["stream"], req["store"])
	}

	// 工具全保留（local_shell 不再被 pivot 丢弃 —— 本次修复的核心）
	tools, _ := req["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools = %d (want 4): %s", len(tools), out)
	}
	if t0, _ := tools[0].(map[string]any); t0["type"] != "local_shell" {
		t.Fatalf("local_shell tool lost: %v", tools[0])
	}

	// input 原样：reasoning item 与 function_call 保留（多轮工具调用推理链不断）
	input, _ := req["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input items = %d (want 4)", len(input))
	}
	if it1, _ := input[1].(map[string]any); it1["type"] != "reasoning" || it1["encrypted_content"] != "gAAAA" {
		t.Fatalf("reasoning item lost: %v", input[1])
	}
	if it2, _ := input[2].(map[string]any); it2["type"] != "function_call" || it2["name"] != "shell" {
		t.Fatalf("function_call item lost: %v", input[2])
	}

	// instructions / reasoning / include 透传；Codex 不支持的采样参数删除
	if req["instructions"] != "You are Codex" {
		t.Fatalf("instructions = %v", req["instructions"])
	}
	if r, _ := req["reasoning"].(map[string]any); r == nil || r["effort"] != "max" {
		t.Fatalf("reasoning passthrough: %v", req["reasoning"])
	}
	if inc, _ := req["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("include passthrough: %v", req["include"])
	}
	if _, ok := req["temperature"]; ok {
		t.Fatalf("temperature must be dropped: %v", req)
	}
	if _, ok := req["parallel_tool_calls"]; ok {
		t.Fatalf("parallel_tool_calls must be dropped: %v", req)
	}
	if req["prompt_cache_key"] != "k-1" {
		t.Fatalf("prompt_cache_key: %v", req)
	}

	// 服务端状态参数必须丢弃（网关无状态）
	if _, ok := req["previous_response_id"]; ok {
		t.Fatal("previous_response_id must be dropped")
	}
	if _, ok := req["store"]; !ok || req["store"] != false {
		t.Fatal("store must be forced to false")
	}
}

// input 为纯字符串（简单请求）也必须可用。
func TestBuildCodexRequestFromResponsesStringInput(t *testing.T) {
	out, err := BuildCodexRequestFromResponses([]byte(`{"model":"m","input":"hi","stream":true}`), "up-m")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	input, _ := req["input"].([]any)
	if len(input) != 1 || req["model"] != "up-m" {
		t.Fatalf("string input: %v", req)
	}
	item, _ := input[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if item["role"] != "user" || part["text"] != "hi" {
		t.Fatalf("string input normalization: %v", input)
	}
	// 无 tools 时不应出现空数组
	if _, ok := req["tools"]; ok {
		t.Fatalf("tools should be absent: %v", req["tools"])
	}
}

// 非法 JSON 必须报错（failover 到下一渠道而不是 500）。
func TestBuildCodexRequestFromResponsesBadJSON(t *testing.T) {
	if _, err := BuildCodexRequestFromResponses([]byte(`not json`), "m"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeCodexRequestInputAndFields(t *testing.T) {
	raw := `{
		"model":"m","input":[
			{"type":"message","id":"msg_old","role":"system","content":"rules"},
			{"type":"item_reference","id":"item_1"},
			{"type":"function_call","id":"fc_old","call_id":"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-too-long","name":"f","arguments":"not json"},
			{"type":"function_call_output","call_id":"call_1","output":{"ok":true}}
		],
		"temperature":1,"top_p":0.2,"parallel_tool_calls":true,"max_output_tokens":10,
		"service_tier":"fast","include":["file_search_call.results"]
	}`
	out, err := BuildCodexRequestFromResponses([]byte(raw), "up")
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls", "max_output_tokens"} {
		if _, ok := req[key]; ok {
			t.Fatalf("unsupported field %s retained: %v", key, req)
		}
	}
	if req["service_tier"] != "priority" || req["instructions"] == "" {
		t.Fatalf("defaults/tier: %v", req)
	}
	reasoning := req["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning defaults: %v", reasoning)
	}
	include := req["include"].([]any)
	if len(include) != 2 || include[1] != "reasoning.encrypted_content" {
		t.Fatalf("include merge: %v", include)
	}
	input := req["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("item_reference must be removed: %v", input)
	}
	msg := input[0].(map[string]any)
	if msg["role"] != "developer" || msg["id"] != nil {
		t.Fatalf("message normalization: %v", msg)
	}
	call := input[1].(map[string]any)
	if len([]rune(call["call_id"].(string))) != 64 || call["arguments"] != "{}" || call["id"] != nil {
		t.Fatalf("function call normalization: %v", call)
	}
	output := input[2].(map[string]any)
	if output["output"] != `{"ok":true}` {
		t.Fatalf("output must be string: %v", output)
	}
}

func TestNormalizeCodexRequestReasoningIncludeAndIdempotency(t *testing.T) {
	body := map[string]any{
		"model": "m", "input": "hi",
		"reasoning": map[string]any{"effort": "none"},
		"include":   []any{"file_search_call.results", "file_search_call.results"},
	}
	normalizeCodexRequest(body)
	first, _ := json.Marshal(body)
	normalizeCodexRequest(body)
	second, _ := json.Marshal(body)
	if string(first) != string(second) {
		t.Fatalf("normalize must be idempotent:\nfirst  %s\nsecond %s", first, second)
	}
	include, _ := body["include"].([]any)
	if len(include) != 1 || include[0] != "file_search_call.results" {
		t.Fatalf("reasoning none must not force encrypted include; existing include should dedupe: %v", include)
	}

	body = map[string]any{
		"model": "m", "input": "hi",
		"include": []any{"reasoning.encrypted_content", "x", "reasoning.encrypted_content", "x"},
	}
	normalizeCodexRequest(body)
	include = body["include"].([]any)
	if len(include) != 2 || include[0] != "reasoning.encrypted_content" || include[1] != "x" {
		t.Fatalf("include merge/dedupe: %v", include)
	}
}

func TestNormalizeCodexToolChoiceValidAndInvalid(t *testing.T) {
	base := func(choice any) map[string]any {
		return map[string]any{
			"model": "m", "input": "hi", "tool_choice": choice,
			"tools": []any{map[string]any{
				"type": "function", "name": "run", "parameters": map[string]any{"type": "object"},
			}},
		}
	}
	for _, choice := range []any{"auto", "none", "required"} {
		body := base(choice)
		normalizeCodexRequest(body)
		if body["tool_choice"] != choice {
			t.Fatalf("valid string choice %v removed: %v", choice, body)
		}
	}
	for _, choice := range []any{"bogus", float64(1), map[string]any{"type": "custom", "name": "missing"}, map[string]any{"type": "function", "name": "missing"}} {
		body := base(choice)
		normalizeCodexRequest(body)
		if _, ok := body["tool_choice"]; ok {
			t.Fatalf("invalid choice retained: %v", body["tool_choice"])
		}
	}
	body := base(map[string]any{"type": "function", "function": map[string]any{"name": "run"}})
	normalizeCodexRequest(body)
	got, _ := body["tool_choice"].(map[string]any)
	if got["type"] != "function" || got["name"] != "run" {
		t.Fatalf("nested valid function choice not normalized: %v", got)
	}

	body = map[string]any{
		"model": "m", "input": "hi",
		"tools":       []any{map[string]any{"type": "custom", "name": "patch"}, map[string]any{"type": "web_search"}},
		"tool_choice": map[string]any{"type": "custom", "name": "patch"},
	}
	normalizeCodexRequest(body)
	got = body["tool_choice"].(map[string]any)
	if got["type"] != "custom" || got["name"] != "patch" {
		t.Fatalf("valid custom choice not normalized: %v", got)
	}
	body["tool_choice"] = map[string]any{"type": "web_search"}
	normalizeCodexRequest(body)
	got = body["tool_choice"].(map[string]any)
	if got["type"] != "web_search" {
		t.Fatalf("valid hosted choice removed: %v", got)
	}
}

func TestNormalizeCodexNamespaceSchema(t *testing.T) {
	body := map[string]any{
		"model": "m", "input": "hi",
		"tools": []any{map[string]any{
			"type": "namespace", "name": "repo",
			"tools": []any{
				map[string]any{"type": "function", "name": "search", "parameters": map[string]any{"type": "string", "pattern": `^\p{L}+$`}},
				map[string]any{"type": "custom", "name": "skip"},
				map[string]any{"type": "function", "name": ""},
			},
		}},
	}
	normalizeCodexRequest(body)
	tools := body["tools"].([]any)
	ns := tools[0].(map[string]any)
	nested := ns["tools"].([]any)
	if len(nested) != 1 {
		t.Fatalf("namespace nested tools not filtered: %v", nested)
	}
	params := nested[0].(map[string]any)["parameters"].(map[string]any)
	if params["type"] != "object" || params["properties"] == nil {
		t.Fatalf("namespace function schema must be object: %v", params)
	}
	if _, ok := params["pattern"]; ok {
		t.Fatalf("namespace schema must clean unsupported pattern: %v", params)
	}
}

func TestNormalizeCodexCallIDTruncationConsistency(t *testing.T) {
	longID := strings.Repeat("调用", 40)
	body := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": longID, "name": "run", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": longID, "output": "ok"},
		},
	}
	normalizeCodexRequest(body)
	items := body["input"].([]any)
	callID := items[0].(map[string]any)["call_id"].(string)
	outputID := items[1].(map[string]any)["call_id"].(string)
	if callID != outputID || len([]rune(callID)) != 64 {
		t.Fatalf("function call/output IDs must share the same rune-safe truncation: %q %q", callID, outputID)
	}
}

func TestNormalizeCodexTools(t *testing.T) {
	longName := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-END-TOO-LONG"
	raw := `{"model":"m","input":"hi","tools":[` +
		`{"type":"function","function":{"name":"` + longName + `","parameters":{"type":"object","properties":{"bad":{"type":"string","pattern":"^\\p{L}+$"},"literal":{"type":"string","pattern":"^\\\\p{L}$"}}}}},` +
		`{"type":"function","name":"flat","parameters":{"type":"object"}},` +
		`{"type":"custom","name":"apply_patch","format":{"type":"grammar"}},` +
		`{"type":"namespace","name":"ns","tools":[{"type":"function","name":"nested","parameters":{"type":"object"}}]},` +
		`{"type":"local_shell"},{"type":"unknown"}` +
		`],"tool_choice":{"type":"function","name":"missing"}}`
	out, err := BuildCodexRequestFromResponses([]byte(raw), "up")
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	tools := req["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tool filtering: %v", tools)
	}
	fn := tools[0].(map[string]any)
	if len([]rune(fn["name"].(string))) != 128 || fn["function"] != nil {
		t.Fatalf("nested function flatten/name: %v", fn)
	}
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if _, ok := props["bad"].(map[string]any)["pattern"]; ok {
		t.Fatalf("unsupported unicode pattern retained: %v", props)
	}
	if props["literal"].(map[string]any)["pattern"] == nil {
		t.Fatalf("escaped literal pattern removed: %v", props)
	}
	flat := tools[1].(map[string]any)
	if flat["parameters"].(map[string]any)["properties"] == nil {
		t.Fatalf("object schema needs properties: %v", flat)
	}
	if _, ok := req["tool_choice"]; ok {
		t.Fatalf("invalid tool choice retained: %v", req["tool_choice"])
	}
}
