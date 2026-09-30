package workbuddy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// stream.go 强制流式（G3）下的本地聚合。
//
// 上游要求 stream:true（wb_proxy.py:3025-3027 无条件设置），因此当客户端要
// 非流式响应时，平台必须自己把 SSE 折成一条 chat/completions JSON，再交给
// relay 既有的非流路径（openaiJSONToAgg / 字节透传）。
//
// 只做折叠，不做协议转换：输出严格是 chat/completions 形态，三个入口
// （chat / messages / responses）各自的后续处理逻辑不变。

// sseMaxBytes 折叠上限：正常对话远小于此；防御上游返回无界流把内存吃满。
const sseMaxBytes = 32 << 20

// sseChunk 上游 SSE 数据块（只声明我们关心的字段）。
type sseChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// CollapseSSE 把上游 SSE 流折叠成单条 chat/completions 响应。
//
// 返回 ok=false 表示「不像 SSE / 没有可用内容」——调用方应保持原样透传，
// 不要把它当成错误（上游可能直接回了 JSON，或流里只有心跳注释）。
func CollapseSSE(data []byte) ([]byte, bool) {
	if len(data) == 0 || len(data) > sseMaxBytes {
		return nil, false
	}
	// 快路径：没有 data: 行就不是 SSE。上游流式体必然带 "data:"。
	if !bytes.Contains(data, []byte("data:")) {
		return nil, false
	}

	var (
		acc      collapsed
		sawChunk bool
		upErr    string
	)
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimRight(raw, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // 空行、event:/id:/注释一律忽略
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ch sseChunk
		if err := json.Unmarshal(payload, &ch); err != nil {
			continue // 单块解析失败不放弃整条流（上游偶尔插入非 JSON 行）
		}
		sawChunk = true
		if ch.Error != nil && ch.Error.Message != "" {
			upErr = ch.Error.Message
		}
		acc.absorb(&ch)
	}
	if !sawChunk {
		return nil, false
	}
	if upErr != "" && acc.content == "" && len(acc.toolCalls) == 0 {
		// 流里只有错误：保持原样，让 relay 的 upstreamFailureDetail 去提取摘要
		return nil, false
	}
	return acc.marshal()
}

// collapsed 折叠累加器。
type collapsed struct {
	id           string
	model        string
	created      int64
	content      string
	reasoning    string
	finishReason string
	toolCalls    []collapsedToolCall
	usage        *sseChunk
}

type collapsedToolCall struct {
	id   string
	name string
	args strings.Builder
}

// absorb 合并一个数据块。工具调用按 index 归并（上游分片下发 id/name/arguments）。
func (c *collapsed) absorb(ch *sseChunk) {
	if c.id == "" {
		c.id = ch.ID
	}
	if c.model == "" {
		c.model = ch.Model
	}
	if c.created == 0 {
		c.created = ch.Created
	}
	if ch.Usage != nil && (ch.Usage.PromptTokens > 0 || ch.Usage.CompletionTokens > 0) {
		c.usage = ch
	}
	for _, choice := range ch.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			c.finishReason = *choice.FinishReason
		}
		if choice.Delta.Content != nil {
			c.content += deltaText(choice.Delta.Content)
		}
		if choice.Delta.ReasoningContent != "" {
			c.reasoning += choice.Delta.ReasoningContent
		}
		// 少数上游把增量放在 message.content（非 delta）——兼容处理
		if choice.Message.Content != "" && choice.Delta.Content == nil {
			c.content += choice.Message.Content
		}
		for _, tc := range choice.Delta.ToolCalls {
			c.absorbToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
	}
}

// absorbToolCall 按 index 归并工具调用分片。
func (c *collapsed) absorbToolCall(index int, id, name, args string) {
	for len(c.toolCalls) <= index {
		c.toolCalls = append(c.toolCalls, collapsedToolCall{})
	}
	tc := &c.toolCalls[index]
	if tc.id == "" {
		tc.id = id
	}
	if tc.name == "" {
		tc.name = name
	}
	tc.args.WriteString(args)
}

// marshal 产出 chat/completions 响应 JSON。
func (c *collapsed) marshal() ([]byte, bool) {
	if c.content == "" && c.reasoning == "" && len(c.toolCalls) == 0 {
		// 有 usage 无内容（如仅统计块）：仍产出空完成，保证记账与客户端语义一致
		if c.usage == nil {
			return nil, false
		}
	}
	message := map[string]any{"role": "assistant"}
	if c.content != "" {
		message["content"] = c.content
	} else if len(c.toolCalls) == 0 {
		message["content"] = ""
	}
	if c.reasoning != "" {
		message["reasoning_content"] = c.reasoning
	}
	if len(c.toolCalls) > 0 {
		calls := make([]map[string]any, 0, len(c.toolCalls))
		for i, tc := range c.toolCalls {
			if tc.name == "" {
				continue // 既无名字又无参数的分片视为噪声
			}
			id := tc.id
			if id == "" {
				id = "call_collapsed_" + itoa(i)
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      tc.name,
					"arguments": tc.args.String(),
				},
			})
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}

	finish := c.finishReason
	if finish == "" {
		if len(c.toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}

	out := map[string]any{
		"id":      orDefault(c.id, "chatcmpl-collapsed"),
		"object":  "chat.completion",
		"created": c.created,
		"model":   c.model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if c.usage != nil && c.usage.Usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     c.usage.Usage.PromptTokens,
			"completion_tokens": c.usage.Usage.CompletionTokens,
			"total_tokens":      c.usage.Usage.TotalTokens,
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}

// deltaText 取增量文本：支持字符串与 [{type:text,text:…}] 两种形态。
func deltaText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, part := range t {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				sb.WriteString(s)
			}
		}
		return sb.String()
	}
	return ""
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// itoa 小整数转字符串（避免为一个用例引入 strconv 的额外分支）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
