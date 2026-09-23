package relay

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
)

// responsesToolBridge 的映射仅属于当前请求，不写入上游请求或共享状态。
// namespace 和自由文本工具在 Chat 中用普通函数表达，返回时恢复原始身份。
type responsesToolIdentity struct {
	Name      string
	Namespace string
	Custom    bool
}

type responsesToolBridge struct {
	tools      []any
	byAlias    map[string]responsesToolIdentity
	byIdentity map[responsesToolIdentity]string
	items      map[string]responsesToolIdentity
}

func newResponsesToolBridge(req map[string]any) *responsesToolBridge {
	b := &responsesToolBridge{byAlias: map[string]responsesToolIdentity{}, byIdentity: map[responsesToolIdentity]string{}, items: map[string]responsesToolIdentity{}}
	reserved := map[string]bool{}
	var reserve func([]any)
	reserve = func(tools []any) {
		for _, raw := range tools {
			t, _ := raw.(map[string]any)
			reserved[str(t["name"])] = true
			children, _ := t["tools"].([]any)
			reserve(children)
		}
	}
	// Responses Lite 将工具定义作为 input item 发送，而不是顶层 tools。
	// 所有消费者（请求转换、历史调用、响应还原）必须使用同一份合并结果。
	topLevel, _ := req["tools"].([]any)
	tools := append([]any(nil), topLevel...)
	input, _ := req["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] == "additional_tools" {
			additional, _ := item["tools"].([]any)
			tools = append(tools, additional...)
		}
	}
	reserve(tools)
	indexes := map[responsesToolIdentity]int{}
	var add func([]any, string)
	add = func(tools []any, ns string) {
		for _, raw := range tools {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name := str(t["name"])
			if t["type"] == "namespace" {
				children, _ := t["tools"].([]any)
				add(children, name)
				continue
			}
			if name == "" || (t["type"] != "function" && t["type"] != "custom") {
				continue
			}
			id := responsesToolIdentity{Name: name, Namespace: ns, Custom: t["type"] == "custom"}
			alias := name
			if existing, ok := b.byIdentity[id]; ok {
				alias = existing
			} else if ns != "" || id.Custom {
				for salt := 0; ; salt++ {
					hash := sha256.Sum256([]byte(fmt.Sprintf("%q/%q/%t/%d", ns, name, id.Custom, salt)))
					alias = fmt.Sprintf("kg_%x", hash[:28])
					if !reserved[alias] {
						break
					}
				}
			}
			reserved[alias] = true
			b.byAlias[alias], b.byIdentity[id] = id, alias
			fn := map[string]any{"name": alias, "parameters": ensureObjectSchema(t["parameters"])}
			if d, ok := t["description"].(string); ok {
				fn["description"] = d
			}
			if strict, ok := t["strict"].(bool); ok {
				fn["strict"] = strict
			}
			if id.Custom {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": "The complete raw text input for this tool."}}, "required": []string{"input"}, "additionalProperties": false}
				fn["description"] = str(t["description"]) + "\nPass the complete raw tool input in the input string."
				if format, ok := t["format"].(map[string]any); ok {
					encoded, _ := json.Marshal(format)
					fn["description"] = str(fn["description"]) + "\nInput format: " + string(encoded)
				}
			}
			// 别名是合法且短于 64 字符的函数名，说明中保留原名便于模型理解。
			if alias != name {
				fn["description"] = ns + "." + name + ": " + str(fn["description"])
			}
			definition := map[string]any{"type": "function", "function": fn}
			if idx, ok := indexes[id]; ok {
				b.tools[idx] = definition
			} else {
				indexes[id] = len(b.tools)
				b.tools = append(b.tools, definition)
			}
		}
	}
	add(tools, "")
	return b
}

func (b *responsesToolBridge) alias(item map[string]any, custom bool) string {
	id := responsesToolIdentity{Name: str(item["name"]), Namespace: str(item["namespace"]), Custom: custom}
	if alias, ok := b.byIdentity[id]; ok {
		return alias
	}
	return id.Name
}

// 历史调用也必须使用与定义相同的别名，自由文本输入包装为 JSON 参数。
func (b *responsesToolBridge) inputItem(raw any) any {
	item, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	switch item["type"] {
	case "function_call", "custom_tool_call":
		custom := item["type"] == "custom_tool_call"
		item["name"] = b.alias(item, custom)
		if custom {
			args, _ := json.Marshal(map[string]any{"input": str(item["input"])})
			item["arguments"] = string(args)
			item["type"] = "function_call"
		}
	case "custom_tool_call_output":
		item["type"] = "function_call_output"
	}
	return item
}

func customToolInput(args string) string {
	var wrapper struct {
		Input string `json:"input"`
	}
	if json.Unmarshal([]byte(args), &wrapper) == nil {
		return wrapper.Input
	}
	return args
}

func (b *responsesToolBridge) restoreItem(item map[string]any) {
	if item["type"] != "function_call" {
		return
	}
	id, ok := b.byAlias[str(item["name"])]
	if !ok {
		return
	}
	b.items[str(item["id"])] = id
	item["name"] = id.Name
	if id.Namespace != "" {
		item["namespace"] = id.Namespace
	}
	if id.Custom {
		item["type"] = "custom_tool_call"
		item["input"] = customToolInput(str(item["arguments"]))
		delete(item, "arguments")
	}
}

func (b *responsesToolBridge) restoreOutput(output []any) {
	for _, raw := range output {
		if item, ok := raw.(map[string]any); ok {
			b.restoreItem(item)
		}
	}
}

// 自由文本包装必须等 JSON 参数收齐才能解码；只缓冲工具输入，文本流照常发送。
func (b *responsesToolBridge) events(event map[string]any) []map[string]any {
	if item, ok := event["item"].(map[string]any); ok {
		b.restoreItem(item)
	}
	if response, ok := event["response"].(map[string]any); ok {
		output, _ := response["output"].([]any)
		b.restoreOutput(output)
	}
	if id, ok := b.items[str(event["item_id"])]; ok && id.Custom {
		switch event["type"] {
		case "response.function_call_arguments.delta":
			return nil
		case "response.function_call_arguments.done":
			input := customToolInput(str(event["arguments"]))
			delete(event, "arguments")
			event["type"], event["input"] = "response.custom_tool_call_input.done", input
			return []map[string]any{{"type": "response.custom_tool_call_input.delta", "item_id": event["item_id"], "output_index": event["output_index"], "delta": input}, event}
		}
	}
	return []map[string]any{event}
}

type responsesToolWriter struct {
	http.ResponseWriter
	bridge *responsesToolBridge
}

func (w *responsesToolWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *responsesToolWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func bridgeResponsesTools(w http.ResponseWriter, raw []byte) http.ResponseWriter {
	var req map[string]any
	if json.Unmarshal(raw, &req) != nil {
		return w
	}
	return &responsesToolWriter{ResponseWriter: w, bridge: newResponsesToolBridge(req)}
}
