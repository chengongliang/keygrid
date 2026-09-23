package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// responses_api.go OpenAI Responses API（/v1/responses）入口协议 ↔ pivot 转换。
//
// 客户端讲 Responses 格式时：
//   - responsesToPivotRequest：请求 → pivot 请求体（渠道无关，一次解析）；
//     上游是 codex/responses 渠道时另有 BuildCodexRequest 直转（不经此处）
//   - writeResponsesJSON：pivot 聚合（relayAgg）→ 非流式 response JSON
//
// 已知取舍（对齐 9router 无状态网关语义）：
//   - previous_response_id / store 等服务端状态参数不支持，丢弃
//   - input 里的 reasoning item 不回传 pivot（无等价字段）
//   - namespace/custom 工具经请求级映射转换为 Chat 函数，返回时还原

// responsesToPivotRequest 解析 Responses 请求体，转成 pivot（chat/completions 形状）。
// 返回 (pivot 请求体, model, stream, err)。
func responsesToPivotRequest(body []byte) ([]byte, string, bool, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", false, fmt.Errorf("responses: parse request: %w", err)
	}
	modelName := strings.TrimSpace(str(req["model"]))
	if modelName == "" {
		return nil, "", false, fmt.Errorf("responses: model required")
	}
	stream, _ := req["stream"].(bool)
	toolBridge := newResponsesToolBridge(req)

	messages := make([]any, 0, 16)
	// instructions → system 消息
	if ins := str(req["instructions"]); ins != "" {
		messages = append(messages, map[string]any{"role": "system", "content": ins})
	}

	switch input := req["input"].(type) {
	case string:
		if input != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, it := range input {
			if msgs := responsesItemToPivotMessages(toolBridge.inputItem(it)); len(msgs) > 0 {
				for _, msg := range msgs {
					// Responses 将并行调用拆成多个 item；Chat 要求同一轮调用
					// 属于同一条 assistant 消息，然后再接各个 tool 结果。
					current, _ := msg.(map[string]any)
					calls, _ := current["tool_calls"].([]any)
					if len(calls) > 0 && len(messages) > 0 {
						previous, _ := messages[len(messages)-1].(map[string]any)
						previousCalls, _ := previous["tool_calls"].([]any)
						if previous["role"] == "assistant" && len(previousCalls) > 0 {
							previous["tool_calls"] = append(previousCalls, calls...)
							continue
						}
					}
					messages = append(messages, msg)
				}
			}
		}
	}

	out := map[string]any{
		"model":    modelName,
		"messages": messages,
		"stream":   stream,
	}

	if len(toolBridge.tools) > 0 {
		out["tools"] = toolBridge.tools
	}

	// tool_choice：responses {type:function,name} → chat {type:function,function:{name}}
	switch tc := req["tool_choice"].(type) {
	case map[string]any:
		if tc["type"] == "function" || tc["type"] == "custom" {
			if name := toolBridge.alias(tc, tc["type"] == "custom"); name != "" {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		} else if s, ok := tc["type"].(string); ok {
			out["tool_choice"] = s // auto/none/required
		}
	case string:
		if tc != "" {
			out["tool_choice"] = tc
		}
	}
	// tools 缺失或全部被丢弃（codex 特有工具如 local_shell 无法映射到 chat）时，
	// tool_choice 必须一并剔除：OpenAI 兼容上游校验 tool_choice 依赖 tools，
	// 否则 400 "When using tool_choice, tools must be set"。
	if tl, ok := out["tools"].([]any); !ok || len(tl) == 0 {
		delete(out, "tool_choice")
	}

	// 公共采样参数透传
	for _, k := range []string{"temperature", "top_p", "parallel_tool_calls", "service_tier", "prompt_cache_key"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	// max_output_tokens → max_tokens
	if v, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	// reasoning → reasoning_effort（chat 侧可消费；summary 等丢弃）。
	// effort 值经 normalizeReasoningEffort 归一化：codex 侧合法的 "max"
	// 等值直接透传会被 chat 上游（DeepSeek/volcengine 等）400 拒绝。
	if r, ok := req["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok && e != "" {
			if v := normalizeReasoningEffort(e); v != "" {
				out["reasoning_effort"] = v
			}
		}
	}

	pivot, err := json.Marshal(out)
	if err != nil {
		return nil, "", false, fmt.Errorf("responses: marshal pivot: %w", err)
	}
	return pivot, modelName, stream, nil
}

// normalizeReasoningEffort 归一化 responses 侧 reasoning effort 为 chat 上游
// 普遍接受的值。codex 允许 model_reasoning_effort=max，但 chat 上游
// （DeepSeek/volcengine 等）只认 low/medium/high/xhigh/none："max" 映射为
// "xhigh"（语义对等），其余未知值返回空（不传该字段，用上游默认），
// 避免整条请求被 400 拒绝。
func normalizeReasoningEffort(e string) string {
	switch e {
	case "low", "medium", "high", "xhigh", "none":
		return e
	case "max":
		return "xhigh"
	default:
		return ""
	}
}

// responsesItemToPivotMessages 单个 input item → 若干 pivot 消息。
func responsesItemToPivotMessages(it any) []any {
	m, ok := it.(map[string]any)
	if !ok {
		return nil
	}
	itemType := str(m["type"])
	// Responses SDK 同时接受省略 type 的简写消息 {role, content}。
	// 转到 chat/anthropic 渠道时也必须保留，否则上游会收到空 messages。
	if itemType == "" && str(m["role"]) != "" {
		itemType = "message"
	}
	switch itemType {
	case "message":
		role, _ := m["role"].(string)
		if role != "user" && role != "assistant" && role != "system" && role != "developer" {
			return nil
		}
		return []any{map[string]any{"role": role, "content": responsesContentToPivot(m["content"])}}
	case "function_call":
		// 历史 function call item → assistant tool_calls 消息
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":   firstNonEmpty(str(m["call_id"]), sanitizeCallID(nil)),
				"type": "function",
				"function": map[string]any{
					"name":      truncUTF8(str(m["name"]), 128),
					"arguments": stringifyArg(m["arguments"]),
				},
			}},
		}}
	case "function_call_output":
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": str(m["call_id"]),
			"content":      stringifyArg(m["output"]),
		}}
	default:
		// reasoning / item_reference 等不参与 pivot
		return nil
	}
}

// responsesContentToPivot responses message content（string 或 parts）→ chat content。
func responsesContentToPivot(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "input_text", "output_text", "text", "refusal":
			text := str(pm["text"])
			if text == "" {
				text = str(pm["refusal"])
			}
			if text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		case "input_image":
			img := map[string]any{"type": "image_url", "image_url": map[string]any{}}
			switch u := pm["image_url"].(type) {
			case string:
				img["image_url"].(map[string]any)["url"] = u
			case map[string]any:
				img["image_url"] = u // {url,detail} 形状与 chat 兼容
			}
			out = append(out, img)
		default:
			if t, ok := pm["text"].(string); ok && t != "" {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		}
	}
	switch len(out) {
	case 0:
		return ""
	case 1:
		// chat 侧 content 只接受「字符串」或「part 数组」两种形态；单 part
		// 裸对象会被上游（openai python SDK/pydantic）按 list 迭代校验直接
		// 400。单文本 part 退化为纯字符串（兼容性最好），其余保持数组。
		if t, ok := out[0].(map[string]any); ok && t["type"] == "text" {
			if txt, ok := t["text"].(string); ok {
				return txt
			}
		}
		return out
	default:
		return out
	}
}

// ---- 响应方向：relayAgg → Responses 非流式 JSON ----

// writeResponsesJSON relayAgg → /v1/responses 非流式响应 JSON。
func writeResponsesJSON(w http.ResponseWriter, model string, agg relayAgg) {
	output := make([]any, 0, len(agg.ToolCalls)+2)
	itemID := func(prefix string) string {
		return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if agg.Reasoning != "" {
		output = append(output, map[string]any{
			"type": "reasoning",
			"id":   itemID("rs"),
			"summary": []any{map[string]any{
				"type": "summary_text", "text": agg.Reasoning,
			}},
		})
	}
	if agg.Content != "" || len(agg.ToolCalls) == 0 {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     itemID("msg"),
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": agg.Content,
			}},
		})
	}
	for _, tc := range agg.ToolCalls {
		if tc.Type == "custom_tool_call" {
			output = append(output, map[string]any{
				"type":    "custom_tool_call",
				"id":      itemID("ctc"),
				"call_id": firstNonEmpty(tc.ID, sanitizeCallID(nil)),
				"name":    tc.Name,
				"input":   tc.Arguments,
				"status":  "completed",
			})
			continue
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        itemID("fc"),
			"call_id":   firstNonEmpty(tc.ID, sanitizeCallID(nil)),
			"name":      tc.Name,
			"arguments": tc.Arguments,
			"status":    "completed",
		})
	}

	if tw, ok := w.(*responsesToolWriter); ok {
		tw.bridge.restoreOutput(output)
	}
	status := "completed"
	incomplete := any(nil)
	if agg.FinishReason == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	respJSON(w, 200, map[string]any{
		"id":                 itemID("resp"),
		"object":             "response",
		"created_at":         time.Now().Unix(),
		"status":             status,
		"model":              model,
		"output":             output,
		"error":              nil,
		"incomplete_details": incomplete,
		"usage": map[string]any{
			"input_tokens":  agg.PromptTokens,
			"output_tokens": agg.CompletionTokens,
			"total_tokens":  agg.PromptTokens + agg.CompletionTokens,
		},
	})
}
