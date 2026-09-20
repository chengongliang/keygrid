package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/quota"
)

// codex.go OpenAI Codex（ChatGPT 订阅）上游协议转换。
//
// Codex 上游不是 OpenAI 兼容的 /v1/chat/completions，而是 ChatGPT backend 的
// Responses API（渠道 base_url 即完整 endpoint：
// https://chatgpt.com/backend-api/codex/responses）。转换规则对齐 9router：
//   - 请求：messages/tools → input/tools（9router 65377.j openai→responses），
//     强制 stream:true + store:false（9router transport forceStream）
//   - 响应：responses SSE → chat.completions chunk（流式透传）或聚合为完整
//     chat.completion JSON（非流式）（9router 4845.t responses→openai）
//
// 鉴权：Bearer access_token + chatgpt-account-id（token extra）+
// originator/User-Agent codex_cli_rs（9router transport.headers）。
// 已知取舍：response_format/stop/seed/max_tokens 系（ChatGPT backend 明确拒绝
// max_output_tokens）等无 Responses 等价映射的参数被丢弃（与 9router 行为一致），
// 需要 JSON mode 的客户端请走 api_key 渠道。

const (
	codexOriginator    = "codex_cli_rs"
	codexUserAgent     = "codex_cli_rs/0.154.0"
	codexAccountHeader = "chatgpt-account-id"

	codexDefaultInstructions = "You are a coding agent. Use the provided tools to complete the user's coding task. Inspect the repository before editing, make only necessary changes, preserve existing user changes, and verify your work with relevant tests."

	// codexSSEMaxLine response.completed 事件携带完整 response 对象（含全部输出
	// 文本与 tool 参数），单行上限对齐 relay 请求体上限。
	codexSSEMaxLine = 16 << 20

	// codexSSEPeekLimit 限制前置探测占用；超过后不再等待，立即无损回放。
	codexSSEPeekLimit = 256 << 10

	// Codex 的 prompt_cache_key/session_id 不应随客户端输入无限增长。
	// 显式值超限时改为稳定摘要，派生值也遵守同一上限。
	codexSessionMaxLength = 64
)

// IsCodexProvider 该渠道是否走 Codex Responses 协议（oauth provider = openai）。
func IsCodexProvider(p *model.Provider) bool {
	return p != nil && p.Kind == "oauth" && p.OAuthProvider == "openai"
}

// NormalizeCodexBaseURL Codex（openai oauth）渠道 base_url 归一化：渠道约定为完整
// Responses endpoint（presets.go 的 https://chatgpt.com/backend-api/codex/responses）。
// 历史默认值与手工填写常漏尾段，POST 到 https://chatgpt.com/backend-api 会拿到
// 非 SSE 响应（客户端表现为 response.completed 前流中断，极难定位）。
// 幂等：已完整的地址与自定义第三方 Responses endpoint 原样返回。
func NormalizeCodexBaseURL(baseURL string) string {
	t := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case t == "":
		return ""
	case strings.HasSuffix(t, "/codex/responses"), strings.HasSuffix(t, "/responses"):
		return t
	case strings.HasSuffix(t, "/backend-api/codex"):
		return t + "/responses"
	case strings.HasSuffix(t, "/backend-api"):
		return t + "/codex/responses"
	}
	return t
}

// ---- 请求转换：chat/completions → responses ----

// BuildCodexRequest 把 OpenAI chat/completions 请求体转成 Codex Responses 请求体。
// upModel 为 model_map 反查后的上游模型名。
func BuildCodexRequest(original []byte, upModel string) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(original, &req); err != nil {
		return nil, fmt.Errorf("codex: parse chat request: %w", err)
	}

	input := make([]any, 0, 16)
	out := map[string]any{
		"model":  upModel,
		"input":  input,
		"stream": true, // 9router forceStream：Codex 上游只按流式消费
		"store":  false,
	}

	instructionsSeen := false
	messages, _ := req["messages"].([]any)
	for _, mi := range messages {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			// 9router：仅首条 system/developer 进 instructions，其余丢弃
			if !instructionsSeen {
				out["instructions"] = joinMessageText(m["content"])
				instructionsSeen = true
			}
		case "user", "assistant":
			if role == "assistant" {
				// assistant 历史：reasoning_content → reasoning item（维持推理上下文）
				if r := reasoningItem(m); r != nil {
					input = append(input, r)
				}
			}
			textType := "input_text"
			if role == "assistant" {
				textType = "output_text"
			}
			parts := contentPartsToResponses(m["content"], textType)
			if len(parts) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    role,
					"content": parts,
				})
			}
			if role == "assistant" {
				// assistant 历史 tool_calls → function_call items
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
						input = append(input, map[string]any{
							"type":      "function_call",
							"call_id":   sanitizeCallID(tc["id"]),
							"name":      truncUTF8(name, 128),
							"arguments": stringifyArg(fn["arguments"]),
						})
					}
				}
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": sanitizeCallID(m["tool_call_id"]),
				"output":  stringifyArg(m["content"]),
			})
		}
	}
	out["input"] = input
	if !instructionsSeen {
		out["instructions"] = ""
	}

	// tools：chat 嵌套 {type:function,function:{...}} → responses 扁平 {type:function,name,...}
	if tools, ok := req["tools"].([]any); ok {
		flat := make([]any, 0, len(tools))
		for _, ti := range tools {
			t, ok := ti.(map[string]any)
			if !ok {
				continue
			}
			if t["type"] != "function" {
				flat = append(flat, t) // 非 function 工具原样透传
				continue
			}
			fn, _ := t["function"].(map[string]any)
			name := strings.TrimSpace(str(fn["name"]))
			if name == "" {
				continue
			}
			ft := map[string]any{
				"type":       "function",
				"name":       truncUTF8(name, 128),
				"parameters": ensureObjectSchema(fn["parameters"]),
			}
			if d, ok := fn["description"].(string); ok {
				ft["description"] = d
			}
			if s, ok := fn["strict"].(bool); ok {
				ft["strict"] = s
			}
			flat = append(flat, ft)
		}
		if len(flat) > 0 {
			out["tools"] = flat
		}
	}

	// Codex 专用字段在最终 normalize 中统一过滤，这里只传递受支持字段。
	for _, k := range []string{"reasoning", "service_tier", "prompt_cache_key", "include", "client_metadata", "text"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	// max_tokens 系直接丢弃：ChatGPT backend 的 Codex Responses 端点只接受固定
	// 参数集，发 max_output_tokens 会 400（Unsupported parameter），真实 Codex CLI
	// 从不携带该参数（官方 api.openai.com/v1/responses 才支持）。输出长度交由
	// 上游默认行为控制。
	// reasoning_effort → reasoning{effort, summary:auto}
	if effort, ok := req["reasoning_effort"].(string); ok && effort != "" {
		if _, exists := out["reasoning"]; !exists {
			out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
		}
	}
	// tool_choice：chat {type:function,function:{name}} → responses {type:function,name}
	switch tc := req["tool_choice"].(type) {
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				out["tool_choice"] = map[string]any{"type": "function", "name": name}
			}
		}
	case string:
		if tc != "" {
			out["tool_choice"] = tc
		}
	}

	normalizeCodexRequest(out)
	return json.Marshal(out)
}

// BuildCodexRequestFromResponses 把 Responses API 请求体直转为 Codex Responses
// 请求体（/v1/responses 入口 + Codex 渠道专用，避免 responses→chat→responses
// 双重有损转换：pivot 方式会丢弃 local_shell/apply_patch 等 Codex 特有工具，
// 导致模型收不到 shell 工具无法执行命令；还会丢 input 里的 reasoning item，
// 推理链在多轮工具调用间断裂）。
// 两侧同为 Responses 形状，只做无状态网关语义的必要改写：
//   - model 替换为渠道 model_map 反查后的上游名
//   - 强制 stream:true + store:false（9router forceStream；Codex 只按流式消费）
//   - 丢弃 previous_response_id（服务端状态，网关不支持）
//   - input/instructions/tools/tool_choice/reasoning 及采样参数原样透传
func BuildCodexRequestFromResponses(original []byte, upModel string) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(original, &req); err != nil {
		return nil, fmt.Errorf("codex: parse responses request: %w", err)
	}

	out := make(map[string]any, len(req)+1)
	for k, v := range req {
		out[k] = v
	}
	out["model"] = upModel
	normalizeCodexRequest(out)
	return json.Marshal(out)
}

var codexAllowedFields = map[string]bool{
	"model": true, "input": true, "instructions": true, "tools": true,
	"tool_choice": true, "stream": true, "store": true, "reasoning": true,
	"service_tier": true, "include": true, "prompt_cache_key": true,
	"client_metadata": true, "text": true,
}

var codexHostedToolTypes = map[string]bool{
	"image_generation": true, "web_search": true, "web_search_preview": true,
	"file_search": true, "computer": true, "computer_use_preview": true,
	"code_interpreter": true, "mcp": true, "local_shell": true,
	"tool_search": true,
}

// normalizeCodexRequest 对两种入口的最终请求做统一、幂等的 Codex 兼容处理。
func normalizeCodexRequest(body map[string]any) {
	body["stream"] = true
	body["store"] = false
	body["input"] = normalizeCodexInput(body["input"])
	body["tools"] = normalizeCodexTools(body["tools"])
	if tools, ok := body["tools"].([]any); !ok || len(tools) == 0 {
		delete(body, "tools")
	}
	normalizeCodexToolChoice(body)

	if strings.TrimSpace(str(body["instructions"])) == "" {
		body["instructions"] = codexDefaultInstructions
	}

	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		reasoning = map[string]any{}
	}
	effort := strings.TrimSpace(str(reasoning["effort"]))
	if effort == "" {
		effort = "low"
	}
	reasoning["effort"] = effort
	summary := strings.TrimSpace(str(reasoning["summary"]))
	if summary == "" {
		summary = "auto"
	}
	reasoning["summary"] = summary
	body["reasoning"] = reasoning
	if effort != "none" {
		body["include"] = mergeStringList(body["include"], "reasoning.encrypted_content")
	} else if include := mergeStringList(body["include"], ""); len(include) > 0 {
		body["include"] = include
	} else {
		delete(body, "include")
	}

	if body["service_tier"] == "fast" {
		body["service_tier"] = "priority"
	} else if body["service_tier"] != nil && body["service_tier"] != "priority" {
		delete(body, "service_tier")
	}
	if cacheKey := boundedCodexSession(str(body["prompt_cache_key"])); cacheKey != "" {
		body["prompt_cache_key"] = cacheKey
	} else {
		delete(body, "prompt_cache_key")
	}
	for k := range body {
		if !codexAllowedFields[k] {
			delete(body, k)
		}
	}
}

// normalizeCodexInput 保证 input 为非空 Responses item 数组，并清理无状态模式下不可解析的引用。
func normalizeCodexInput(v any) []any {
	items := make([]any, 0)
	switch input := v.(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			items = append(items, codexTextInput(input))
		}
	case []any:
		for _, raw := range input {
			if text, ok := raw.(string); ok {
				if strings.TrimSpace(text) != "" {
					items = append(items, codexTextInput(text))
				}
				continue
			}
			item, ok := raw.(map[string]any)
			if !ok || item["type"] == "item_reference" {
				continue
			}
			if item["role"] == "system" && (item["type"] == nil || item["type"] == "message") {
				item["role"] = "developer"
			}
			if id := str(item["id"]); hasCodexServerIDPrefix(id) {
				delete(item, "id")
			}
			switch item["type"] {
			case "function_call":
				item["call_id"] = normalizeCallID(item["call_id"])
				item["arguments"] = validJSONObjectString(item["arguments"])
			case "function_call_output", "custom_tool_call_output":
				item["call_id"] = normalizeCallID(item["call_id"])
				item["output"] = stringifyArg(item["output"])
			case "custom_tool_call":
				item["call_id"] = normalizeCallID(item["call_id"])
				item["input"] = stringifyArg(item["input"])
			}
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		items = append(items, codexTextInput("..."))
	}
	return items
}

func codexTextInput(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

func hasCodexServerIDPrefix(id string) bool {
	for _, prefix := range []string{"rs_", "fc_", "msg_", "resp_"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func normalizeCallID(v any) string {
	return truncUTF8(sanitizeCallID(v), 64)
}

func validJSONObjectString(v any) string {
	s := strings.TrimSpace(stringifyArg(v))
	if s == "" {
		return "{}"
	}
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil || obj == nil {
		return "{}"
	}
	return s
}

// normalizeCodexTools 接受 Chat 嵌套与 Responses 扁平 function，并过滤 Codex 不支持的工具。
func normalizeCodexTools(v any) []any {
	rawTools, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(rawTools))
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName := str(tool["type"])
		switch typeName {
		case "function":
			fn, _ := tool["function"].(map[string]any)
			name := strings.TrimSpace(str(tool["name"]))
			if name == "" {
				name = strings.TrimSpace(str(fn["name"]))
			}
			if name == "" {
				continue
			}
			clean := map[string]any{"type": "function", "name": truncUTF8(name, 128)}
			description := str(tool["description"])
			if description == "" {
				description = str(fn["description"])
			}
			if description != "" {
				clean["description"] = description
			}
			params := tool["parameters"]
			if _, ok := params.(map[string]any); !ok {
				params = fn["parameters"]
			}
			clean["parameters"] = cleanCodexSchema(ensureObjectSchema(params))
			if strict, ok := tool["strict"].(bool); ok {
				clean["strict"] = strict
			} else if strict, ok := fn["strict"].(bool); ok {
				clean["strict"] = strict
			}
			out = append(out, clean)
		case "custom":
			name := strings.TrimSpace(str(tool["name"]))
			if name == "" {
				continue
			}
			tool["name"] = truncUTF8(name, 128)
			out = append(out, tool)
		case "namespace":
			name := strings.TrimSpace(str(tool["name"]))
			if name == "" {
				continue
			}
			tool["name"] = truncUTF8(name, 128)
			nested, _ := tool["tools"].([]any)
			cleanNested := make([]any, 0, len(nested))
			for _, rawNested := range nested {
				nestedTool, ok := rawNested.(map[string]any)
				if !ok || nestedTool["type"] != "function" {
					continue
				}
				nestedName := strings.TrimSpace(str(nestedTool["name"]))
				if nestedName == "" {
					continue
				}
				nestedTool["name"] = truncUTF8(nestedName, 128)
				nestedTool["parameters"] = cleanCodexSchema(ensureObjectSchema(nestedTool["parameters"]))
				cleanNested = append(cleanNested, nestedTool)
			}
			if len(cleanNested) == 0 {
				continue
			}
			tool["tools"] = cleanNested
			out = append(out, tool)
		default:
			if codexHostedToolTypes[typeName] {
				out = append(out, tool)
			}
		}
	}
	return out
}

func normalizeCodexToolChoice(body map[string]any) {
	if choice, ok := body["tool_choice"].(string); ok {
		switch choice {
		case "auto", "none", "required":
			return
		default:
			delete(body, "tool_choice")
			return
		}
	}
	choice, ok := body["tool_choice"].(map[string]any)
	if !ok {
		delete(body, "tool_choice")
		return
	}
	typeName := strings.TrimSpace(str(choice["type"]))
	if typeName == "function" || typeName == "custom" {
		name := strings.TrimSpace(str(choice["name"]))
		if name == "" && typeName == "function" {
			if fn, ok := choice["function"].(map[string]any); ok {
				name = strings.TrimSpace(str(fn["name"]))
			}
		}
		name = truncUTF8(name, 128)
		if name == "" || !hasCodexTool(body["tools"], typeName, name) {
			delete(body, "tool_choice")
			return
		}
		body["tool_choice"] = map[string]any{"type": typeName, "name": name}
		return
	}
	if !codexHostedToolTypes[typeName] || !hasCodexTool(body["tools"], typeName, "") {
		delete(body, "tool_choice")
	}
}

func hasCodexTool(v any, typeName, name string) bool {
	tools, ok := v.([]any)
	if !ok {
		return false
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || tool["type"] != typeName {
			continue
		}
		if name == "" || tool["name"] == name {
			return true
		}
	}
	return false
}

func cleanCodexSchema(v any) any {
	switch node := v.(type) {
	case []any:
		for i := range node {
			node[i] = cleanCodexSchema(node[i])
		}
	case map[string]any:
		for key, value := range node {
			if key == "pattern" {
				if pattern, ok := value.(string); ok && hasUnescapedUnicodePropertyEscape(pattern) {
					delete(node, key)
					continue
				}
			}
			if key == "properties" {
				if properties, ok := value.(map[string]any); ok {
					for property, schema := range properties {
						properties[property] = cleanCodexSchema(schema)
					}
					continue
				}
			}
			node[key] = cleanCodexSchema(value)
		}
	}
	return v
}

// hasUnescapedUnicodePropertyEscape 仅识别奇数个反斜杠引出的 \\p{} / \\P{}。
func hasUnescapedUnicodePropertyEscape(pattern string) bool {
	for i := 0; i+2 < len(pattern); i++ {
		if pattern[i] != '\\' || (pattern[i+1] != 'p' && pattern[i+1] != 'P') || pattern[i+2] != '{' {
			continue
		}
		backslashes := 1
		for j := i - 1; j >= 0 && pattern[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			return true
		}
	}
	return false
}

func mergeStringList(v any, required string) []any {
	out := make([]any, 0)
	seen := make(map[string]bool)
	if values, ok := v.([]any); ok {
		for _, value := range values {
			s, ok := value.(string)
			if !ok || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	if required != "" && !seen[required] {
		out = append(out, required)
	}
	return out
}

// resolveCodexSession 按 body、header、匿名派生值的顺序生成稳定会话标识。
func resolveCodexSession(r *http.Request, body []byte, userID, apiKeyID, providerID int64) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		for _, key := range []string{"prompt_cache_key", "session_id", "conversation_id"} {
			if value := boundedCodexSession(str(payload[key])); value != "" {
				return value
			}
		}
	}
	if r != nil {
		for _, key := range []string{"x-session-id", "session-id", "session_id", "x-amp-thread-id", "x-client-request-id"} {
			if value := boundedCodexSession(r.Header.Get(key)); value != "" {
				return value
			}
		}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("keygrid-codex-session:%d:%d:%d", userID, apiKeyID, providerID)))
	return "kg_" + hex.EncodeToString(sum[:])[:codexSessionMaxLength-len("kg_")]
}

func boundedCodexSession(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= codexSessionMaxLength && !strings.ContainsAny(value, "\r\n") {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	encoded := hex.EncodeToString(sum[:])
	return "kg_" + encoded[:codexSessionMaxLength-len("kg_")]
}

func injectCodexSession(body []byte, sessionID string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("codex: inject session: %w", err)
	}
	cacheKey := boundedCodexSession(str(payload["prompt_cache_key"]))
	if cacheKey == "" {
		cacheKey = boundedCodexSession(sessionID)
	}
	payload["prompt_cache_key"] = cacheKey
	return json.Marshal(payload)
}

func codexSessionFromBody(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return boundedCodexSession(str(payload["prompt_cache_key"]))
}

// str any → string。
func str(v any) string {
	s, _ := v.(string)
	return s
}

// joinMessageText string content 原样；数组 content 取各 part 的 text 拼接。
func joinMessageText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := pm["text"].(string); ok && t != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// contentPartsToResponses chat content → responses content parts（text/image）。
func contentPartsToResponses(content any, textType string) []any {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []any{map[string]any{"type": textType, "text": s}}
	}
	parts, ok := content.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "text":
			out = append(out, map[string]any{"type": textType, "text": str(pm["text"])})
		case "image_url":
			// chat: {image_url: "https://..."} 或 {image_url:{url,detail}} → responses input_image
			img := map[string]any{"type": "input_image"}
			switch u := pm["image_url"].(type) {
			case string:
				img["image_url"] = u
			case map[string]any:
				img["image_url"] = str(u["url"])
				if d, ok := u["detail"].(string); ok && d != "" {
					img["detail"] = d
				} else {
					img["detail"] = "auto"
				}
			}
			if s, _ := img["image_url"].(string); s != "" {
				out = append(out, img)
			}
		case "input_image":
			out = append(out, pm)
		default:
			// 未知 part：尽力取 text/content 序列化（9router 同策略）
			if t, ok := pm["text"].(string); ok {
				out = append(out, map[string]any{"type": textType, "text": t})
				continue
			}
			if c, ok := pm["content"]; ok {
				out = append(out, map[string]any{"type": textType, "text": stringifyArg(c)})
			}
		}
	}
	return out
}

// reasoningItem assistant 消息里的 reasoning 内容 → responses reasoning item
// （9router extractReasoning；客户端按 deepseek 风格回传 reasoning_content）。
func reasoningItem(m map[string]any) map[string]any {
	text := ""
	switch v := m["reasoning_content"].(type) {
	case string:
		text = v
	case nil:
	default:
		text = stringifyArg(v)
	}
	if text == "" {
		if s, ok := m["reasoning"].(string); ok {
			text = s
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return map[string]any{
		"type":    "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
}

// sanitizeCallID call_id 清洗（9router N8：非空字符串原样，空则生成）。
func sanitizeCallID(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return "call_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// stringifyArg 任意 JSON 值 → 字符串（string 原样，其余序列化）。
func stringifyArg(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

// ensureObjectSchema responses 工具 parameters 必须是带 properties 的 object schema。
func ensureObjectSchema(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if str(m["type"]) == "object" {
		if _, ok := m["properties"].(map[string]any); ok {
			return m
		}
	}
	cp := make(map[string]any, len(m)+2)
	for k, val := range m {
		cp[k] = val
	}
	cp["type"] = "object"
	if _, ok := cp["properties"].(map[string]any); !ok {
		cp["properties"] = map[string]any{}
	}
	return cp
}

// truncUTF8 按 rune 截断（避免把多字节字符切成两半）。
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- 响应转换：responses SSE → openai ----

// aggToolCall 聚合出的单个工具调用。
type aggToolCall struct {
	ID        string
	Name      string
	Type      string
	Arguments string // function 为 JSON arguments；custom 为原始 input
}

// relayAgg 非流式聚合结果（relay 非流式回包与渠道探测共用）。
type relayAgg struct {
	Content          string
	Reasoning        string
	ToolCalls        []aggToolCall
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
	ErrMsg           string // response.failed / error 事件
	HasToolCall      bool
	// pendingTools item_id → 聚合中的工具调用（output_item.added 登记，
	// arguments delta 累加，output_item.done 定稿）
	pendingTools map[string]*aggToolCall
}

// parseCodexSSE 从 responses SSE 流中逐事件回调。
// data 行解析为 {type, ...fields}（responses SSE 的 event/data 双行里只消费 data）。
// 聚合状态由回调方维护；返回扫描错误（回调返回的 error 原样透出）。
func parseCodexSSE(r io.Reader, onEvent func(typeName string, data map[string]any) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), codexSSEMaxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // event:/注释/空行跳过
		}
		payload := strings.TrimSpace(string(line[5:]))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		typeName, _ := ev["type"].(string)
		if err := onEvent(typeName, ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("codex sse read: %w", err)
	}
	return nil
}

// isToolCallItem 判断 output item 是否为工具调用。
func isToolCallItem(item map[string]any) bool {
	t, _ := item["type"].(string)
	return t == "function_call" || t == "custom_tool_call"
}

// applyCodexEvent 把一条 responses 事件累加进聚合结果（非流式/探测共用）。
func applyCodexEvent(agg *relayAgg, typeName string, ev map[string]any) {
	d, _ := ev["delta"].(string)
	switch typeName {
	case "response.output_text.delta":
		agg.Content += d
	case "response.reasoning_summary_text.delta":
		if agg.Reasoning != "" {
			agg.Reasoning += "\n"
		}
		agg.Reasoning += d
	case "response.output_item.added":
		item, _ := ev["item"].(map[string]any)
		if !isToolCallItem(item) {
			return
		}
		agg.HasToolCall = true
		agg.FinishReason = "tool_calls"
		iid := itemEventID(ev, item)
		if agg.pendingTools == nil {
			agg.pendingTools = map[string]*aggToolCall{}
		}
		agg.pendingTools[iid] = &aggToolCall{
			ID:   str(item["call_id"]),
			Name: str(item["name"]),
			Type: str(item["type"]),
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if tc := agg.pendingTool(ev); tc != nil {
			tc.Arguments += d
		}
	case "response.output_item.done":
		item, _ := ev["item"].(map[string]any)
		if !isToolCallItem(item) {
			return
		}
		iid := itemEventID(ev, item)
		tc := agg.pendingTools[iid]
		if tc == nil {
			tc = &aggToolCall{}
		}
		// name/call_id 以 done 里的为准（added 可能缺字段）
		if v := str(item["call_id"]); v != "" {
			tc.ID = v
		}
		if v := str(item["name"]); v != "" {
			tc.Name = v
		}
		if v := str(item["type"]); v != "" {
			tc.Type = v
		}
		// 上游没发 delta 时从 done 的完整参数补齐（custom_tool_call 字段是 input）
		if tc.Arguments == "" {
			if v := str(item["arguments"]); v != "" {
				tc.Arguments = v
			} else {
				tc.Arguments = stringifyArg(item["input"])
			}
		}
		delete(agg.pendingTools, iid)
		agg.ToolCalls = append(agg.ToolCalls, *tc)
	case "response.completed", "response.done":
		// 收尾：极端情况下上游不发 output_item.done，把 pending 全部落账
		for iid, tc := range agg.pendingTools {
			agg.ToolCalls = append(agg.ToolCalls, *tc)
			delete(agg.pendingTools, iid)
		}
		if resp, ok := ev["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				agg.PromptTokens = intNum(u["input_tokens"])
				agg.CompletionTokens = intNum(u["output_tokens"])
			}
		}
	case "response.failed", "error":
		agg.ErrMsg = codexErrorMessage(ev)
	}
}

// itemEventID output item 的稳定标识（item.id 优先，回退 item_id）。
func itemEventID(ev map[string]any, item map[string]any) string {
	if v := str(item["id"]); v != "" {
		return v
	}
	return str(ev["item_id"])
}

// pendingTool 按 item_id 取聚合中的工具调用。
func (agg *relayAgg) pendingTool(ev map[string]any) *aggToolCall {
	iid := str(ev["item_id"])
	if iid == "" {
		return nil
	}
	return agg.pendingTools[iid]
}

// codexErrorMessage 提取失败事件 message（error.message 与 response.error.message 两种形状）。
func codexErrorMessage(ev map[string]any) string {
	msg := "codex upstream error"
	if m, ok := ev["message"].(string); ok && m != "" {
		return m
	}
	if e, ok := ev["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok && m != "" {
			return m
		}
	}
	if resp, ok := ev["response"].(map[string]any); ok {
		if e, ok := resp["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok && m != "" {
				return m
			}
		}
	}
	return msg
}

// intNum json 数值 → int。
func intNum(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// CodexStreamAgg 流式聚合结果的导出视图（供 handlers 等外部调用方逐块消费）。
type CodexStreamAgg struct {
	Content          string
	Reasoning        string
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
	ErrMsg           string // response.failed / error 事件携带的错误信息
}

// AggregateCodexStream 拉取完整 responses SSE 并聚合（非流式请求/渠道测试用）。
func AggregateCodexStream(r io.Reader) (*relayAgg, error) {
	agg := &relayAgg{FinishReason: "stop"}
	err := parseCodexSSE(r, func(typeName string, ev map[string]any) error {
		applyCodexEvent(agg, typeName, ev)
		return nil
	})
	return agg, err
}

// AggregateCodexStreamOnDelta 带逐事件增量回调的 Codex SSE 聚合（流式打字机）。
// content/reasoning 均为追加语义，onDelta 以「上次写入边界 → 本次写入边界」切片
// 传导出本次事件新增的增量，不会截断多字节字符；onDelta 为 nil 时与
// AggregateCodexStream 等价。返回聚合结果与扫描错误。
func AggregateCodexStreamOnDelta(r io.Reader, onDelta func(contentDelta, reasoningDelta string)) (*CodexStreamAgg, error) {
	agg := &relayAgg{FinishReason: "stop"}
	err := parseCodexSSE(r, func(typeName string, ev map[string]any) error {
		cLen, rLen := len(agg.Content), len(agg.Reasoning)
		applyCodexEvent(agg, typeName, ev)
		if onDelta != nil && (len(agg.Content) > cLen || len(agg.Reasoning) > rLen) {
			onDelta(agg.Content[cLen:], agg.Reasoning[rLen:])
		}
		return nil
	})
	out := &CodexStreamAgg{
		Content:          agg.Content,
		Reasoning:        agg.Reasoning,
		FinishReason:     agg.FinishReason,
		PromptTokens:     agg.PromptTokens,
		CompletionTokens: agg.CompletionTokens,
		ErrMsg:           agg.ErrMsg,
	}
	return out, err
}

// ---- 上游调用 ----

// codexUpstreamHeaders Codex 上游请求头（9router transport.headers + chatgpt-account-id）。
func codexUpstreamHeaders(token, accountID, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "text/event-stream")
	h.Set("originator", codexOriginator)
	h.Set("User-Agent", codexUserAgent)
	if accountID != "" {
		h.Set(codexAccountHeader, accountID)
	}
	if sessionID != "" {
		h.Set("session_id", sessionID)
	}
	return h
}

// upstreamCallCodex 执行一次 Codex Responses 调用：
//   - 上游始终按流式（stream:true）请求；
//   - 入口 chat：responses SSE → chat.completions chunks 转换透传 / 聚合为完整 JSON；
//   - 入口 responses：原始 SSE 透传 / 聚合为完整 response JSON；
//   - 入口 messages：responses SSE → anthropic 事件桥接 / 聚合为完整 message JSON。
//
// client 由调用方按渠道代理开关选定（clientFor）。
// provider 非空时做 x-codex-* 响应头被动观察（额度快照；测试可传 nil 跳过）。
// 返回 (HTTP 状态码, usage, 错误)。错误发生在写出任何字节之前时可 failover。
func (h *Handler) upstreamCallCodex(
	ctx context.Context,
	client *http.Client,
	target string,
	token, accountID string,
	body []byte,
	clientStream bool,
	entry string,
	w http.ResponseWriter,
	provider *model.Provider,
	ua string,
) (int, UsageRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return 0, UsageRecord{}, err
	}
	for k, vs := range codexUpstreamHeaders(token, accountID, codexSessionFromBody(body)) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// UA 策略：默认（ua 为空）保持 codexUpstreamHeaders 的固定 codex_cli_rs；
	// 渠道显式配 custom/forward 时覆盖（上游强依赖该头，谨慎使用）。
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	upResp, err := client.Do(req)
	if err != nil {
		return 0, UsageRecord{}, err
	}
	defer upResp.Body.Close()

	// 被动观察：上游响应头携带额度水印（x-codex-*）时合并进快照（cli-proxy-api
	// quota signals 同款思路）；429 限流响应同样带头（limit_reached 水印）。
	// 异步落库，不阻塞转发热路径。
	if h.QuotaSyncer != nil && provider != nil && quota.HasQuotaHeaders(upResp.Header) {
		h.QuotaSyncer.ObserveHeaders(provider, upResp.Header)
	}

	if upResp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(upResp.Body, 4<<10))
		return upResp.StatusCode, UsageRecord{}, &UpstreamError{Status: upResp.StatusCode, Body: string(b), ContentType: upResp.Header.Get("Content-Type")}
	}

	// Codex 偶尔以 HTTP 200 返回流内过载/容量错误。必须在写客户端响应头前
	// 探测，否则 handler 已无法切换到同一用户的下一个渠道。
	if status, err := peekCodexSSEError(upResp); err != nil {
		return status, UsageRecord{}, err
	}

	if clientStream {
		switch entry {
		case protoResponses:
			// 格式一致：原始 responses SSE 透传（usage 提取已兼容嵌套形状）
			return streamPassthrough(w, upResp)
		case protoAnthropic:
			return bridgeCodexSSEToMessages(w, upResp, upModelFromBody(body))
		default:
			return streamCodexChunks(w, upResp)
		}
	}

	agg, err := AggregateCodexStream(upResp.Body)
	if err != nil {
		return upResp.StatusCode, UsageRecord{}, err
	}
	if agg.ErrMsg != "" && agg.Content == "" && len(agg.ToolCalls) == 0 {
		return upResp.StatusCode, UsageRecord{}, fmt.Errorf("codex: %s", agg.ErrMsg)
	}
	usage := UsageRecord{PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens, Found: agg.PromptTokens > 0 || agg.CompletionTokens > 0}
	writeEntryJSON(w, entry, upModelFromBody(body), *agg)
	return upResp.StatusCode, usage, nil
}

var codexSSETransientPatterns = []string{
	"server_is_overloaded",
	"service_unavailable_error",
	"selected model is at capacity",
	"model_at_capacity",
}

var codexSSEUserOutputTypes = map[string]bool{
	"response.output_text.delta":             true,
	"response.function_call_arguments.delta": true,
	"response.custom_tool_call_input.delta":  true,
}

// codexNonSSEError Codex 渠道上游 HTTP 200 但响应体不是 SSE 事件流：绝大多数情况是
// 渠道 base_url 指到了非 Responses endpoint（如只写到 https://chatgpt.com/backend-api）。
// Error() 只给诊断（可写日志/错误链），Detail() 给上游正文摘要（仅回调用方，勿落库）。
type codexNonSSEError struct {
	status      int
	contentType string
	detail      string
}

func (e *codexNonSSEError) Error() string {
	return fmt.Sprintf("codex upstream returned non-SSE response (http %d, content-type %q); check channel base_url", e.status, e.contentType)
}

// Detail 上游响应摘要（可能含请求回显，仅供发起请求的调用方排查）。
func (e *codexNonSSEError) Detail() string { return e.detail }

// codexSSEFrameLike 事件是否像一条 SSE 帧：取首个非空行，字段名需是 data/event/id/retry
// 或注释行 ":"（WHATWG SSE 仅定义这几个字段，未知字段名一律忽略）。
// HTML/JSON/纯文本错误页会被判为非 SSE。
func codexSSEFrameLike(event []byte) bool {
	for _, raw := range bytes.Split(event, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		if line[0] == ':' { // 注释/心跳行
			return true
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return false
		}
		switch strings.ToLower(string(bytes.TrimSpace(line[:colon]))) {
		case "data", "event", "id", "retry":
			return true
		}
		return false
	}
	return true // 全是空行：没有判定为非 SSE 的证据
}

// codexBodySnippet 上游正文摘要（先按字节截断再空白压缩），仅回给调用方排查。
func codexBodySnippet(b []byte) string {
	const maxBytes = 512
	if len(b) > maxBytes {
		b = b[:maxBytes]
	}
	return truncUTF8(strings.Join(strings.Fields(string(b)), " "), 200)
}

// peekCodexSSEError 在 HTTP 200 Codex SSE 首个用户可见输出前探测可重试错误。
// 每次按完整 SSE 事件读取，response.created、in_progress 和 reasoning 事件不会
// 提前结束探测；正常输出、EOF 或字节上限会把已读前缀与剩余 Body 无损拼回。
func peekCodexSSEError(upResp *http.Response) (int, error) {
	if upResp == nil || upResp.StatusCode != http.StatusOK || upResp.Body == nil {
		return 0, nil
	}

	original := upResp.Body
	limited := &io.LimitedReader{R: original, N: codexSSEPeekLimit}
	reader := bufio.NewReader(limited)
	var prefix bytes.Buffer

	for prefix.Len() < codexSSEPeekLimit {
		event, readErr := readCodexSSEEvent(reader)
		if len(event) > 0 {
			// 200 但不是 SSE 事件流：多半渠道 base_url 指到了非 Responses
			// endpoint（历史默认值 https://chatgpt.com/backend-api 就属此类）。
			// 必须在写出任何字节前明确报错，否则非 SSE 正文会被当 SSE 透传，
			// 客户端只看到「response.completed 前流中断」，无法定位。
			if !codexSSEFrameLike(event) {
				return http.StatusBadGateway, &codexNonSSEError{
					status:      upResp.StatusCode,
					contentType: upResp.Header.Get("Content-Type"),
					detail:      codexBodySnippet(event),
				}
			}
			_, _ = prefix.Write(event)
			// 用户可见输出一旦开始，本次请求就不能再 failover；即便正文恰好
			// 包含暂态错误关键词，也必须优先按正常输出回放。
			if codexSSEEventHasUserOutput(event) {
				break
			}
			if message, matched := codexSSEPreOutputError(event); matched {
				return http.StatusServiceUnavailable, fmt.Errorf("codex sse: %s", message)
			}
		}
		if readErr != nil {
			// EOF 是正常结束；其他读取错误交给后续流处理，且已读字节仍需回放。
			break
		}
	}

	// reader 可能缓存 limited 中尚未回放的字节；limited 耗尽后再接 original，
	// 既不会丢字节，也不会重复读取。Close 仍转发到原始响应体。
	upResp.Body = &codexReplayBody{
		Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader, original),
		Closer: original,
	}
	return 0, nil
}

type codexReplayBody struct {
	io.Reader
	io.Closer
}

// readCodexSSEEvent 读取一个以空行结束的完整 SSE event。外层 LimitedReader
// 保证最多主动读取 codexSSEPeekLimit 字节，触及上限即以 EOF 返回并回放。
func readCodexSSEEvent(reader *bufio.Reader) ([]byte, error) {
	var event bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			event.WriteString(line)
			if line == "\n" || line == "\r\n" {
				return event.Bytes(), err
			}
		}
		if err != nil {
			return event.Bytes(), err
		}
	}
}

func codexSSEEventHasUserOutput(event []byte) bool {
	typeName, _, ok := decodeCodexSSEEvent(event)
	return ok && codexSSEUserOutputTypes[typeName]
}

// codexSSEPreOutputError 同时识别明确失败事件和已知暂态文本。部分上游把
// 错误类型放在嵌套字段而不是 event/type 中，因此文本模式作为可靠兜底。
func codexSSEPreOutputError(event []byte) (string, bool) {
	typeName, ev, parsed := decodeCodexSSEEvent(event)
	lower := strings.ToLower(string(event))
	pattern := ""
	for _, candidate := range codexSSETransientPatterns {
		if strings.Contains(lower, candidate) {
			pattern = candidate
			break
		}
	}
	if typeName != "response.failed" && typeName != "error" && pattern == "" {
		return "", false
	}
	if parsed {
		if message := codexErrorMessage(ev); message != "codex upstream error" {
			return message, true
		}
	}
	if pattern != "" {
		return pattern, true
	}
	return "codex upstream error", true
}

func decodeCodexSSEEvent(event []byte) (string, map[string]any, bool) {
	var eventType string
	for _, raw := range bytes.Split(event, []byte{'\n'}) {
		line := bytes.TrimSuffix(raw, []byte{'\r'})
		if bytes.HasPrefix(line, []byte("event:")) {
			eventType = strings.TrimSpace(string(line[len("event:"):]))
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev map[string]any
		if json.Unmarshal(payload, &ev) != nil {
			continue
		}
		if typeName := str(ev["type"]); typeName != "" {
			eventType = typeName
		}
		return eventType, ev, true
	}
	return eventType, nil, eventType != ""
}

// upModelFromBody 从已转换的请求体里取模型名（记账展示用）。
func upModelFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// writeChatCompletionJSON 聚合结果 → 完整 chat.completion JSON。
func writeChatCompletionJSON(w http.ResponseWriter, model string, agg relayAgg) {
	msg := map[string]any{"role": "assistant"}
	msg["content"] = nil
	if agg.Content != "" {
		msg["content"] = agg.Content
	}
	if agg.Reasoning != "" {
		msg["reasoning_content"] = agg.Reasoning
	}
	if len(agg.ToolCalls) > 0 {
		tcs := make([]any, 0, len(agg.ToolCalls))
		for i, tc := range agg.ToolCalls {
			arguments := tc.Arguments
			if tc.Type == "custom_tool_call" {
				encoded, _ := json.Marshal(map[string]string{"input": tc.Arguments})
				arguments = string(encoded)
			}
			tcs = append(tcs, map[string]any{
				"id":   firstNonEmpty(tc.ID, sanitizeCallID(nil)),
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": arguments,
				},
				"index": i,
			})
		}
		msg["tool_calls"] = tcs
	}
	finish := agg.FinishReason
	if finish == "" {
		finish = "stop"
	}
	respJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-codex-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     agg.PromptTokens,
			"completion_tokens": agg.CompletionTokens,
			"total_tokens":      agg.PromptTokens + agg.CompletionTokens,
		},
	})
}

// streamCodexChunks responses SSE → chat.completions chunks 逐块转换透传。
// 语义与 streamPassthrough 一致：headers 写出后不可 failover；客户端断开返回 errClientGone。
func streamCodexChunks(w http.ResponseWriter, upResp *http.Response) (int, UsageRecord, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, UsageRecord{}, fmt.Errorf("streaming unsupported by writer")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(upResp.StatusCode)

	st := newCodexChunkState()
	var lastUsage UsageRecord
	var streamErr error

	aggErr := parseCodexSSE(upResp.Body, func(typeName string, ev map[string]any) error {
		chunk, usage, fin := convertCodexEvent(st, typeName, ev)
		if usage != nil {
			lastUsage = *usage
		}
		if chunk != nil {
			if err := writeSSEData(w, chunk); err != nil {
				streamErr = errClientGone
				return errClientGone
			}
			flusher.Flush()
		}
		if fin {
			// 终止块后补 [DONE]
			if err := writeSSEDone(w); err != nil {
				streamErr = errClientGone
				return errClientGone
			}
			flusher.Flush()
		}
		return nil
	})
	// 上游异常中断也补 [DONE]，避免客户端挂起
	if !st.finishSent {
		_ = writeSSEDone(w)
		flusher.Flush()
	}
	if streamErr == nil && aggErr != nil && !errors.Is(aggErr, errClientGone) {
		// 上游读中断：流已开始，无法 failover；透出读取错误
		streamErr = aggErr
	}
	return upResp.StatusCode, lastUsage, streamErr
}

// writeSSEData 写一条 data: {json} 行。
func writeSSEData(w io.Writer, chunk map[string]any) error {
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
	return err
}

// writeSSEDone 写 data: [DONE]。
func writeSSEDone(w io.Writer) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// codexChunkState 流式转换状态（对齐 9router responses→openai state）。
type codexChunkState struct {
	id string
	// created/model 兜底自 response.created 事件
	created int64
	model   string
	// roleSent OpenAI 流式约定首个 delta 携带 role（含 reasoning/tool-only 流）
	roleSent bool
	// toolIndex 已分配的 chat tool_calls index
	toolIndex int
	// toolIdxByItem item_id → chat tool_calls index
	toolIdxByItem map[string]int
	// argsEmittedIdx 已通过 delta（或 done 补发）写出参数的 index ——
	// 防止 output_item.done 的完整参数与 deltas 重复拼接导致 JSON 损坏
	argsEmittedIdx map[int]bool
	toolTypeByIdx  map[int]string
	finishReason   string
	finishSent     bool
}

func newCodexChunkState() *codexChunkState {
	return &codexChunkState{
		id:             "chatcmpl-codex-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		created:        time.Now().Unix(),
		toolIdxByItem:  map[string]int{},
		argsEmittedIdx: map[int]bool{},
		toolTypeByIdx:  map[int]string{},
		finishReason:   "stop",
	}
}

// withRole 首个 delta 附带 role:"assistant"。
func (s *codexChunkState) withRole(delta map[string]any) map[string]any {
	if !s.roleSent {
		delta["role"] = "assistant"
		s.roleSent = true
	}
	return delta
}

// chunkBase 组装 chunk 骨架。
func (s *codexChunkState) chunkBase(delta map[string]any, finish *string) map[string]any {
	return map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}

// convertCodexEvent 单条 responses 事件 → chat chunk（usage 非空表示捕获到记账；
// fin 表示终止块已写）。
func convertCodexEvent(s *codexChunkState, typeName string, ev map[string]any) (chunk map[string]any, usage *UsageRecord, fin bool) {
	if s.model == "" {
		s.model = upModelOfEvent(ev)
	}
	switch typeName {
	case "response.output_text.delta":
		d, _ := ev["delta"].(string)
		if d == "" {
			return nil, nil, false
		}
		delta := s.withRole(map[string]any{})
		delta["content"] = d
		return s.chunkBase(delta, nil), nil, false

	case "response.reasoning_summary_text.delta":
		d, _ := ev["delta"].(string)
		if d == "" {
			return nil, nil, false
		}
		delta := s.withRole(map[string]any{})
		delta["reasoning_content"] = d
		return s.chunkBase(delta, nil), nil, false

	case "response.output_item.added":
		item, _ := ev["item"].(map[string]any)
		if !isToolCallItem(item) {
			return nil, nil, false
		}
		idx := s.toolIndex
		s.toolIndex++
		if iid := itemEventID(ev, item); iid != "" {
			s.toolIdxByItem[iid] = idx
		}
		s.finishReason = "tool_calls"
		s.toolTypeByIdx[idx] = str(item["type"])
		arguments := ""
		if s.toolTypeByIdx[idx] == "custom_tool_call" {
			arguments = `{"input":"`
		}
		delta := s.withRole(map[string]any{
			"tool_calls": []any{map[string]any{
				"index": idx,
				"id":    firstNonEmpty(str(item["call_id"]), sanitizeCallID(nil)),
				"type":  "function",
				"function": map[string]any{
					"name":      truncUTF8(str(item["name"]), 128),
					"arguments": arguments,
				},
			}},
		})
		return s.chunkBase(delta, nil), nil, false

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		d, _ := ev["delta"].(string)
		if d == "" {
			return nil, nil, false
		}
		idx := s.toolIndexFor(ev)
		s.argsEmittedIdx[idx] = true // done 补发据此去重
		if typeName == "response.custom_tool_call_input.delta" {
			d = jsonStringFragment(d)
		}
		delta := s.withRole(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    idx,
				"function": map[string]any{"arguments": d},
			}},
		})
		return s.chunkBase(delta, nil), nil, false

	case "response.output_item.done":
		item, _ := ev["item"].(map[string]any)
		if !isToolCallItem(item) {
			return nil, nil, false
		}
		// 上游不发 arguments delta 只发 done 时补发完整参数（custom_tool_call 字段是 input）
		args := str(item["arguments"])
		if args == "" {
			args = stringifyArg(item["input"])
		}
		idx := s.toolIndexFor(ev)
		if s.toolTypeByIdx[idx] == "custom_tool_call" {
			if s.argsEmittedIdx[idx] {
				args = `"}`
			} else {
				args = `{"input":"` + jsonStringFragment(args) + `"}`
			}
		} else if args == "" || s.argsEmittedIdx[idx] {
			return nil, nil, false
		}
		s.argsEmittedIdx[idx] = true
		delta := s.withRole(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    idx,
				"function": map[string]any{"arguments": args},
			}},
		})
		return s.chunkBase(delta, nil), nil, false

	case "response.completed", "response.done":
		if resp, ok := ev["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				usage = &UsageRecord{
					PromptTokens:     intNum(u["input_tokens"]),
					CompletionTokens: intNum(u["output_tokens"]),
					Model:            s.model,
					Found:            true,
				}
			}
		}
		if !s.finishSent {
			s.finishSent = true
			fr := s.finishReason
			return s.chunkBase(map[string]any{}, &fr), usage, true
		}
		return nil, usage, false

	case "response.failed", "error":
		if !s.finishSent {
			s.finishSent = true
			fr := "stop"
			chunk = s.chunkBase(s.withRole(map[string]any{"content": "[Error] " + codexErrorMessage(ev)}), &fr)
			return chunk, nil, true
		}
		return nil, nil, false
	}
	return nil, nil, false
}

func jsonStringFragment(value string) string {
	encoded, _ := json.Marshal(value)
	if len(encoded) < 2 {
		return ""
	}
	return string(encoded[1 : len(encoded)-1])
}

// toolIndexFor item_id → chat tool index（未知 item 取最近一个）。
func (s *codexChunkState) toolIndexFor(ev map[string]any) int {
	item, _ := ev["item"].(map[string]any)
	if iid := itemEventID(ev, item); iid != "" {
		if idx, ok := s.toolIdxByItem[iid]; ok {
			return idx
		}
	}
	idx := s.toolIndex - 1
	if idx < 0 {
		idx = 0
	}
	return idx
}

// upModelOfEvent 从事件里尽力取模型名（response.created 带 response.model）。
func upModelOfEvent(ev map[string]any) string {
	if resp, ok := ev["response"].(map[string]any); ok {
		if m, ok := resp["model"].(string); ok && m != "" {
			return m
		}
	}
	return ""
}

// firstNonEmpty 第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
