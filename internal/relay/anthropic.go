package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

// anthropic.go Anthropic Messages 协议（/v1/messages）与 pivot（OpenAI chat/completions）
// 的双向转换。
//
// 入口方向（客户端讲 anthropic 协议）：
//   - anthropicToPivotRequest：/v1/messages 请求 → pivot 请求体（渠道无关，一次解析）
//   - 响应侧：pivot 聚合（relayAgg）→ anthropic message JSON（writeAnthropicMessageJSON）
//
// 渠道方向（渠道是 anthropic 协议上游，如 minimax-cn / kimi coding / Claude 官方）：
//   - pivotToAnthropicRequest：pivot 请求体 → /v1/messages 上游请求体
//   - anthropicJSONToAgg：上游非流式响应 → relayAgg（记账 + 回转入口格式）
//
// 内容块映射（对齐 9router anthropic↔openai 转换）：
//   text          ↔ {type:text}
//   image(base64) ↔ {type:image_url,image_url:{url:"data:...;base64,..."}}（往返还原 source）
//   image(url)    ↔ {type:image_url,image_url:{url}}（按 data:/http(s) 前缀区分往返形状）
//   tool_use      ↔ assistant.tool_calls[{id,name,arguments}]
//   tool_result   ↔ {role:tool,tool_call_id,content}
//   thinking      ↔ reasoning_content（仅入口→pivot 保留；反向丢弃——anthropic 上游
//                    要求 thinking 块带 signature，无签名的回传会被拒绝）
//
// stop_reason ↔ finish_reason：end_turn/stop_sequence→stop；max_tokens→length；tool_use→tool_calls

const anthropicVersion = "2023-06-01"

// channelProto 渠道上游协议：responses（codex）/ anthropic / openai（默认）。
func channelProto(p *model.Provider) string {
	if IsCodexProvider(p) {
		return protoResponses
	}
	if p != nil && p.Protocol == "anthropic" {
		return protoAnthropic
	}
	return protoOpenAI
}

// ---- stop_reason ↔ finish_reason ----

// finishFromAnthropicStop anthropic stop_reason → chat finish_reason。
func finishFromAnthropicStop(sr string) string {
	switch sr {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		// end_turn / stop_sequence / pause_turn / refusal / 未知 → stop
		return "stop"
	}
}

// anthropicStopFromFinish chat finish_reason → anthropic stop_reason。
func anthropicStopFromFinish(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ---- 入口方向：/v1/messages 请求 → pivot ----

// anthropicToPivotRequest 解析 anthropic /v1/messages 请求体，转成 pivot（openai
// chat/completions 形状）。返回 (pivot 请求体, model, stream, err)。
func anthropicToPivotRequest(body []byte) ([]byte, string, bool, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", false, fmt.Errorf("anthropic: parse request: %w", err)
	}
	modelName := strings.TrimSpace(str(req["model"]))
	if modelName == "" {
		return nil, "", false, fmt.Errorf("anthropic: model required")
	}
	stream, _ := req["stream"].(bool)

	messages := make([]any, 0, 16)
	// system（string 或 content blocks 数组）→ 首条 system 消息
	if sys := anthropicSystemText(req["system"]); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}

	msgs, _ := req["messages"].([]any)
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		// anthropic 消息可能同时携带 text / tool_use / tool_result 块，chat 格式
		// 需要拆成多条消息（tool_result → role=tool，其余留在原角色）。
		out := anthropicMessageToPivot(role, m["content"])
		messages = append(messages, out...)
	}

	out := map[string]any{
		"model":    modelName,
		"messages": messages,
		"stream":   stream,
	}
	anthropicParamsToPivot(req, out)
	pivot, err := json.Marshal(out)
	if err != nil {
		return nil, "", false, fmt.Errorf("anthropic: marshal pivot: %w", err)
	}
	return pivot, modelName, stream, nil
}

// anthropicSystemText system 字段 → 纯文本（string 原样；blocks 取 text 以空行拼接）。
func anthropicSystemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		var parts []string
		for _, b := range s {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := bm["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

// anthropicMessageToPivot 单条 anthropic 消息 → 若干 pivot 消息（tool_result 块拆出
// 为独立 role=tool 消息）。
func anthropicMessageToPivot(role string, content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}

	var texts []any     // text/image blocks（留在原角色）
	var toolCalls []any // tool_use blocks（assistant → chat tool_calls）
	var reasoning strings.Builder
	var out []any

	// emitToolResult 把 tool_result 块转成 role=tool 消息。
	emitToolResult := func(bm map[string]any) {
		tid := str(bm["tool_use_id"])
		out = append(out, map[string]any{
			"role":         "tool",
			"tool_call_id": tid,
			"content":      anthropicBlockContentText(bm["content"]),
		})
	}

	for _, bi := range blocks {
		bm, ok := bi.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			texts = append(texts, map[string]any{"type": "text", "text": str(bm["text"])})
		case "image":
			if img := anthropicImageToPivot(bm["source"]); img != nil {
				texts = append(texts, img)
			}
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   str(bm["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      truncUTF8(str(bm["name"]), 128),
					"arguments": stringifyArg(bm["input"]),
				},
			})
		case "tool_result":
			emitToolResult(bm)
		case "thinking":
			if t := str(bm["thinking"]); t != "" {
				if reasoning.Len() > 0 {
					reasoning.WriteByte('\n')
				}
				reasoning.WriteString(t)
			}
		default:
			// redacted_thinking 等无 pivot 等价物：丢弃
		}
	}

	// 先 tool 消息（保持 anthropic 里 tool_result 通常紧跟前置 assistant 的顺序感），
	// 再本角色消息 —— 顺序对 chat 上游语义无影响，这里保持产出顺序稳定即可。
	msg := map[string]any{"role": role}
	switch {
	case len(texts) == 1:
		// 单个 text block 展开为纯字符串（chat 语义）
		if tm, ok := texts[0].(map[string]any); ok && tm["type"] == "text" {
			msg["content"] = tm["text"]
		} else {
			msg["content"] = texts[0]
		}
	case len(texts) > 1:
		msg["content"] = texts
	default:
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) == 0 && len(texts) == 0 && reasoning.Len() == 0 {
		return out // 纯 tool_result 消息
	}
	return append(out, msg)
}

// anthropicImageToPivot anthropic image source → chat image_url part。
func anthropicImageToPivot(source any) map[string]any {
	sm, ok := source.(map[string]any)
	if !ok {
		return nil
	}
	switch sm["type"] {
	case "base64":
		media := str(sm["media_type"])
		data := str(sm["data"])
		if media == "" || data == "" {
			return nil
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:" + media + ";base64," + data,
		}}
	case "url":
		u := str(sm["url"])
		if u == "" {
			return nil
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}
	default:
		return nil
	}
}

// anthropicBlockContentText tool_result.content（string 或 blocks）→ 纯文本。
func anthropicBlockContentText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	blocks, ok := v.([]any)
	if !ok {
		return stringifyArg(v)
	}
	var parts []string
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			parts = append(parts, str(bm["text"]))
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicParamsToPivot 顶层采样/工具参数 → pivot 字段。
func anthropicParamsToPivot(req, out map[string]any) {
	if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := req["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		out["top_p"] = v
	}
	if ss, ok := req["stop_sequences"].([]any); ok && len(ss) > 0 {
		out["stop"] = ss
	}
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		chatTools := make([]any, 0, len(tools))
		for _, ti := range tools {
			t, ok := ti.(map[string]any)
			if !ok {
				continue
			}
			name := strings.TrimSpace(str(t["name"]))
			if name == "" {
				continue
			}
			ct := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       truncUTF8(name, 128),
					"parameters": ensureObjectSchema(t["input_schema"]),
				},
			}
			if d, ok := t["description"].(string); ok && d != "" {
				ct["description"] = d
			}
			chatTools = append(chatTools, ct)
		}
		if len(chatTools) > 0 {
			out["tools"] = chatTools
		}
	}
	switch tc := req["tool_choice"].(type) {
	case map[string]any:
		switch tc["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if name := str(tc["name"]); name != "" {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		}
	case string:
		if tc != "" {
			out["tool_choice"] = tc
		}
	}
	// tools 缺失或全部被丢弃时,tool_choice 一并剔除：上游校验 tool_choice 依赖
	// tools（chat 形状同 responses→chat，见 responsesToPivotRequest）。
	if tl, ok := out["tools"].([]any); !ok || len(tl) == 0 {
		delete(out, "tool_choice")
	}
	// metadata / thinking / top_k：无 pivot 等价物，丢弃
}

// ---- 渠道方向：pivot → /v1/messages 上游请求 ----

// pivotToAnthropicRequest 把 pivot（chat/completions 形状）请求体转成 anthropic
// 上游请求体。upModel 为 model_map 反查后的上游模型名。
func pivotToAnthropicRequest(original []byte, upModel string) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(original, &req); err != nil {
		return nil, fmt.Errorf("anthropic: parse pivot request: %w", err)
	}

	out := map[string]any{
		"model":      upModel,
		"max_tokens": anthropicMaxTokens(req),
	}
	if s, ok := req["stream"].(bool); ok {
		out["stream"] = s
	} else {
		out["stream"] = false
	}

	var systemParts []string
	messages := make([]any, 0, 16)
	// tool_result 专用 user 消息（连续 tool 消息合并进同一条 user）
	var pendingUserBlocks []any
	flushPending := func() {
		if len(pendingUserBlocks) > 0 {
			messages = append(messages, map[string]any{"role": "user", "content": pendingUserBlocks})
			pendingUserBlocks = nil
		}
	}

	msgs, _ := req["messages"].([]any)
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if t := joinMessageText(m["content"]); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			flushPending()
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": pivotContentToAnthropicBlocks(m["content"]),
			})
		case "assistant":
			flushPending()
			blocks := pivotContentToAnthropicBlocks(m["content"])
			// assistant 历史 tool_calls → tool_use blocks（thinking 不回传：无 signature）
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					name := strings.TrimSpace(str(fn["name"]))
					if name == "" {
						continue
					}
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    firstNonEmpty(str(tc["id"]), sanitizeCallID(nil)),
						"name":  truncUTF8(name, 128),
						"input": jsonRawObject(fn["arguments"]),
					})
				}
			}
			if len(blocks) == 0 {
				continue // 空消息 anthropic 会拒绝
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			// chat tool 消息 → tool_result block（挂到专用 user 消息）
			blocks := pendingUserBlocks
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": firstNonEmpty(str(m["tool_call_id"]), sanitizeCallID(nil)),
				"content":     anthropicBlockContentText(m["content"]),
			})
			pendingUserBlocks = blocks
		}
	}
	flushPending()
	out["messages"] = messages
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}

	// tools → anthropic 形状
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		at := make([]any, 0, len(tools))
		for _, ti := range tools {
			t, ok := ti.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := t["function"].(map[string]any)
			name := strings.TrimSpace(str(fn["name"]))
			if name == "" {
				continue
			}
			at2 := map[string]any{
				"name":         truncUTF8(name, 128),
				"input_schema": ensureObjectSchema(fn["parameters"]),
			}
			if d, ok := fn["description"].(string); ok && d != "" {
				at2["description"] = d
			}
			at = append(at, at2)
		}
		if len(at) > 0 {
			out["tools"] = at
		}
	}

	// tool_choice → anthropic 形状
	switch tc := req["tool_choice"].(type) {
	case string:
		switch tc {
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		}
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if name := str(fn["name"]); name != "" {
				out["tool_choice"] = map[string]any{"type": "tool", "name": name}
			}
		}
	}
	// anthropic 上游同样校验 tool_choice 依赖 tools：无 tools 时剔除，避免 400
	if tl, ok := out["tools"].([]any); !ok || len(tl) == 0 {
		delete(out, "tool_choice")
	}

	if v, ok := req["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		out["top_p"] = v
	}
	if stop, ok := req["stop"].([]any); ok && len(stop) > 0 {
		out["stop_sequences"] = stop
	}

	return json.Marshal(out)
}

// anthropicMaxTokens anthropic 必填 max_tokens：取 pivot 显式值，缺省 4096。
func anthropicMaxTokens(req map[string]any) any {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := req[k]; ok {
			if n, ok := v.(float64); ok && n > 0 {
				return int(n)
			}
		}
	}
	return 4096
}

// jsonRawObject JSON 字符串/值 → object（tool_use.input 必须是对象）。
func jsonRawObject(v any) map[string]any {
	switch t := v.(type) {
	case string:
		if t == "" {
			return map[string]any{}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(t), &m); err == nil {
			return m
		}
		return map[string]any{}
	case map[string]any:
		return t
	default:
		return map[string]any{}
	}
}

// pivotContentToAnthropicBlocks chat content → anthropic content blocks。
func pivotContentToAnthropicBlocks(content any) []any {
	blocks := make([]any, 0, 8)
	appendText := func(s string) {
		if s != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": s})
		}
	}
	if s, ok := content.(string); ok {
		appendText(s)
		return blocks
	}
	parts, ok := content.([]any)
	if !ok {
		return blocks
	}
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "text":
			appendText(str(pm["text"]))
		case "image_url":
			blocks = append(blocks, pivotImageToAnthropic(pm["image_url"]))
		}
	}
	return blocks
}

// pivotImageToAnthropic chat image_url → anthropic image block（data URL 还原为
// base64 source，http(s) URL 用 url source）。
func pivotImageToAnthropic(imageURL any) map[string]any {
	u := ""
	switch t := imageURL.(type) {
	case string:
		u = t
	case map[string]any:
		u = str(t["url"])
	}
	if u == "" {
		return map[string]any{"type": "text", "text": "[unsupported image]"}
	}
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		// data:<media>;base64,<data>
		media, data, ok2 := strings.Cut(rest, ";base64,")
		if ok2 && media != "" && data != "" {
			return map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": media, "data": data,
			}}
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
}

// upstreamTargetAnthropic 构建上游 anthropic /v1/messages URL。
func upstreamTargetAnthropic(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/messages"
}

// anthropicUpstreamHeaders anthropic 上游请求头。鉴权规则：
//   - oauth provider=anthropic（Claude Pro）：Bearer + anthropic-beta oauth flag
//   - 其它 oauth（如 kimi coding）：Bearer + x-api-key 双带（兼容两类端点）
//   - api_key（如 minimax）：x-api-key
//   - anthropic-version 一律带上
func anthropicUpstreamHeaders(p *model.Provider, secret string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("anthropic-version", anthropicVersion)
	if p != nil && p.Kind == "oauth" {
		h.Set("Authorization", "Bearer "+secret)
		if p.OAuthProvider == "anthropic" {
			h.Set("anthropic-beta", "oauth-2025-04-20")
		} else {
			h.Set("x-api-key", secret)
		}
		return h
	}
	h.Set("x-api-key", secret)
	return h
}

// upstreamCallAnthropic 执行一次 anthropic 协议上游调用：
//   - 客户端流式：入口 messages 透传原始 SSE；入口 chat/responses 走桥转换；
//   - 客户端非流式：入口 messages 字节透传；入口 chat/responses 解析后回转。
func (h *Handler) upstreamCallAnthropic(
	ctx context.Context,
	client *http.Client,
	target string,
	p *model.Provider,
	secret string,
	body []byte,
	clientStream bool,
	entry string,
	w http.ResponseWriter,
) (int, UsageRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return 0, UsageRecord{}, err
	}
	for k, vs := range anthropicUpstreamHeaders(p, secret) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if clientStream {
		req.Header.Set("Accept", "text/event-stream")
	}

	upResp, err := client.Do(req)
	if err != nil {
		return 0, UsageRecord{}, err
	}
	defer upResp.Body.Close()

	if upResp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(upResp.Body, 4<<10))
		return upResp.StatusCode, UsageRecord{}, &UpstreamError{Status: upResp.StatusCode, Body: string(b)}
	}

	upModel := upModelFromBody(body)
	switch {
	case clientStream && entry == protoAnthropic:
		// 格式一致：原始 SSE 透传（usage 提取已兼容 anthropic 形状）
		return streamPassthrough(w, upResp)
	case clientStream:
		return bridgeAnthropicSSEToEntry(w, upResp, entry, upModel)
	case entry == protoAnthropic:
		// 格式一致：字节透传
		respBytes, err := io.ReadAll(upResp.Body)
		if err != nil {
			return upResp.StatusCode, UsageRecord{}, err
		}
		writeRawJSON(w, upResp.StatusCode, respBytes)
		u := UsageRecord{}
		if agg, ok := anthropicJSONToAgg(respBytes, upModel); ok {
			u = UsageRecord{PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens,
				Found: agg.PromptTokens > 0 || agg.CompletionTokens > 0}
		}
		return upResp.StatusCode, u, nil
	default:
		respBytes, err := io.ReadAll(upResp.Body)
		if err != nil {
			return upResp.StatusCode, UsageRecord{}, err
		}
		agg, ok := anthropicJSONToAgg(respBytes, upModel)
		if !ok {
			writeRawJSON(w, upResp.StatusCode, respBytes)
			return upResp.StatusCode, UsageRecord{}, nil
		}
		if agg.ErrMsg != "" && agg.Content == "" && len(agg.ToolCalls) == 0 {
			return upResp.StatusCode, UsageRecord{}, fmt.Errorf("anthropic: %s", agg.ErrMsg)
		}
		usage := UsageRecord{PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens,
			Found: agg.PromptTokens > 0 || agg.CompletionTokens > 0}
		writeEntryJSON(w, entry, upModel, agg)
		return upResp.StatusCode, usage, nil
	}
}

// ---- 响应方向：anthropic 非流式 JSON ↔ relayAgg ----

// anthropicJSONToAgg anthropic /v1/messages 非流式响应 → relayAgg。
func anthropicJSONToAgg(data []byte, model string) (relayAgg, bool) {
	var r struct {
		Type       string `json:"type"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return relayAgg{}, false
	}
	if r.Error != nil && r.Error.Message != "" {
		return relayAgg{ErrMsg: r.Error.Message}, true
	}
	agg := relayAgg{FinishReason: finishFromAnthropicStop(r.StopReason)}
	if m := firstNonEmpty(r.Model, model); m != "" {
		_ = m // 聚合不携带 model，调用方负责展示名
	}
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			agg.Content += b.Text
		case "thinking":
			if agg.Reasoning != "" {
				agg.Reasoning += "\n"
			}
			agg.Reasoning += b.Thinking
		case "tool_use":
			agg.HasToolCall = true
			agg.ToolCalls = append(agg.ToolCalls, aggToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: stringifyArg(json.RawMessage(b.Input)),
			})
		}
	}
	if r.Usage != nil {
		agg.PromptTokens = r.Usage.InputTokens
		agg.CompletionTokens = r.Usage.OutputTokens
	}
	if agg.FinishReason == "" {
		agg.FinishReason = "stop"
	}
	return agg, true
}

// writeAnthropicMessageJSON relayAgg → anthropic /v1/messages 非流式响应 JSON。
func writeAnthropicMessageJSON(w http.ResponseWriter, model string, agg relayAgg) {
	content := make([]any, 0, len(agg.ToolCalls)+2)
	if agg.Reasoning != "" {
		// 无 signature 的 thinking 块：仅为信息不丢失；严格客户端可忽略
		content = append(content, map[string]any{"type": "thinking", "thinking": agg.Reasoning})
	}
	if agg.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": agg.Content})
	}
	for _, tc := range agg.ToolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(tc.ID, sanitizeCallID(nil)),
			"name":  tc.Name,
			"input": jsonRawObject(tc.Arguments),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	respJSON(w, 200, map[string]any{
		"id":            "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStopFromFinish(agg.FinishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  agg.PromptTokens,
			"output_tokens": agg.CompletionTokens,
		},
	})
}
