package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Codex 0.155.1 的 Responses Lite 把工具放在 input.additional_tools 中，
// 没有顶层 tools 并不代表客户端没有提供工具。
func TestResponsesLiteAdditionalToolsReachChat(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-luna","input":[
		{"type":"additional_tools","id":"tools_1","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[
				{"type":"custom","name":"exec","description":"Execute JavaScript using local tools"},
				{"type":"function","name":"wait","parameters":{"type":"object","properties":{}}}
			]}
		]},
		{"role":"user","content":"Create weather.txt"}
	],"tool_choice":"auto"}`)
	pivot, _, _, err := responsesToPivotRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(pivot, &req); err != nil {
		t.Fatal(err)
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("Responses Lite file tools lost: want 2, got %s", pivot)
	}
	if req["tool_choice"] != "auto" {
		t.Fatal("tool_choice was lost")
	}
	if len(req["messages"].([]any)) != 1 {
		t.Fatal("additional_tools must not become a chat message")
	}
}

// Codex 的文件工具可能位于命名空间内，兼容渠道必须把它们交给模型。
func TestResponsesNamespaceToolsReachChat(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-luna","input":"write a file","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},{"type":"custom","name":"apply_patch","description":"Apply a patch"}]}]}`)
	pivot, _, _, err := responsesToPivotRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(pivot, &req); err != nil {
		t.Fatal(err)
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("Codex file tools lost: want 2 tools, got %s", pivot)
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["type"] != "function" {
			t.Fatalf("chat upstream needs function tools: %v", tool)
		}
	}
}

// 使用真实上游调用和 SSE 桥，覆盖定义→调用→客户端执行结果→下一轮请求。
func TestResponsesFileToolsRoundTrip(t *testing.T) {
	for _, lite := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("lite=%t/stream=%t", lite, stream), func(t *testing.T) {
				patch := "*** Begin Patch\n*** Add File: hello.txt\n+你好 \"Codex\"\\test\n*** End Patch"
				request := map[string]any{
					"model": "gpt-5.6-luna", "stream": stream, "input": "write hello.txt",
					"tools": []any{
						map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
						map[string]any{"type": "namespace", "name": "functions", "tools": []any{
							map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
							map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "text"}},
						}},
						map[string]any{"type": "namespace", "name": "other", "tools": []any{
							map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
						}},
					},
					"tool_choice": map[string]any{"type": "custom", "name": "apply_patch", "namespace": "functions"},
				}
				var additional map[string]any
				if lite {
					additional = map[string]any{"type": "additional_tools", "role": "developer", "tools": request["tools"]}
					request["input"] = []any{additional, map[string]any{"role": "user", "content": "write hello.txt"}}
					delete(request, "tools")
				}
				raw, _ := json.Marshal(request)
				pivot, _, _, err := responsesToPivotRequest(raw)
				if err != nil {
					t.Fatal(err)
				}
				var up map[string]any
				_ = json.Unmarshal(pivot, &up)
				defs := up["tools"].([]any)
				aliases := make([]string, len(defs))
				seen := map[string]bool{}
				for i, def := range defs {
					fn := def.(map[string]any)["function"].(map[string]any)
					aliases[i] = str(fn["name"])
					if seen[aliases[i]] || len(aliases[i]) > 64 {
						t.Fatalf("invalid/colliding alias: %q", aliases[i])
					}
					seen[aliases[i]] = true
				}
				choice := up["tool_choice"].(map[string]any)["function"].(map[string]any)
				if choice["name"] != aliases[2] {
					t.Fatalf("tool_choice not mapped: %v", choice)
				}
				wrapped, _ := json.Marshal(map[string]any{"input": patch})
				args := []string{`{"cmd":"pwd"}`, string(wrapped)}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var received map[string]any
					if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
						t.Error(err)
						return
					}
					if len(received["tools"].([]any)) != 4 {
						t.Error("tools lost at HTTP boundary")
					}
					calls := []any{}
					for i, alias := range aliases[1:3] {
						calls = append(calls, map[string]any{"index": i, "id": fmt.Sprintf("call_%d", i), "type": "function", "function": map[string]any{"name": alias, "arguments": args[i]}})
					}
					if !stream {
						_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": calls}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20}})
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for i, rawCall := range calls {
						call := rawCall.(map[string]any)
						fn := call["function"].(map[string]any)
						fn["arguments"] = ""
						chunk := func(delta any, finish any) {
							b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": finish}}})
							fmt.Fprintf(w, "data: %s\n\n", b)
						}
						chunk(map[string]any{"tool_calls": []any{call}}, nil)
						// 刻意拆开 JSON 转义序列，验证自由文本不会以包装后的 JSON 发给客户端。
						for _, part := range []string{args[i][:7], args[i][7:]} {
							chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": part}}}}, nil)
						}
					}
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				}))
				defer server.Close()
				recorder := httptest.NewRecorder()
				h := &Handler{}
				status, _, err := h.upstreamCall(context.Background(), server.Client(), server.URL, "test", pivot, stream, protoResponses, bridgeResponsesTools(recorder, raw), "")
				if err != nil || status != 200 {
					t.Fatalf("upstream: %d %v", status, err)
				}
				var response map[string]any
				if stream {
					var customDelta string
					var added, done bool
					for _, line := range strings.Split(recorder.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
							continue
						}
						switch event["type"] {
						case "response.output_item.added":
							item := event["item"].(map[string]any)
							if item["type"] == "custom_tool_call" {
								added = item["name"] == "apply_patch" && item["namespace"] == "functions"
							}
						case "response.custom_tool_call_input.delta":
							customDelta += str(event["delta"])
						case "response.custom_tool_call_input.done":
							done = event["input"] == patch
						case "response.completed":
							response = event["response"].(map[string]any)
						}
					}
					if !added || !done || customDelta != patch {
						t.Fatalf("custom SSE round trip failed: %s", recorder.Body.String())
					}
				} else if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				output, _ := response["output"].([]any)
				if len(output) != 2 {
					t.Fatalf("missing tool output: %v", response)
				}
				function := output[0].(map[string]any)
				custom := output[1].(map[string]any)
				if function["name"] != "exec_command" || function["namespace"] != "functions" || function["arguments"] != args[0] {
					t.Fatalf("function identity lost: %v", function)
				}
				if custom["type"] != "custom_tool_call" || custom["name"] != "apply_patch" || custom["namespace"] != "functions" || custom["input"] != patch {
					t.Fatalf("custom input lost: %v", custom)
				}
				request["input"] = append(output,
					map[string]any{"type": "function_call_output", "call_id": "call_0", "output": "repo"},
					map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "Success"})
				if lite {
					request["input"] = append([]any{additional}, request["input"].([]any)...)
				}
				next, _ := json.Marshal(request)
				nextPivot, _, _, err := responsesToPivotRequest(next)
				if err != nil {
					t.Fatal(err)
				}
				_ = json.Unmarshal(nextPivot, &up)
				messages := up["messages"].([]any)
				if len(messages) != 3 {
					t.Fatalf("tool history lost: %s", nextPivot)
				}
				calls := messages[0].(map[string]any)["tool_calls"].([]any)
				if len(calls) != 2 {
					t.Fatalf("parallel calls must share an assistant message: %s", nextPivot)
				}
				for i := 0; i < 2; i++ {
					call := calls[i].(map[string]any)
					fn := call["function"].(map[string]any)
					if fn["name"] != aliases[i+1] || fn["arguments"] != args[i] {
						t.Fatalf("history identity/input changed: %v", fn)
					}
					result := messages[i+1].(map[string]any)
					if result["role"] != "tool" || result["tool_call_id"] != fmt.Sprintf("call_%d", i) {
						t.Fatalf("tool result lost: %v", result)
					}
				}
			})
		}
	}
}
