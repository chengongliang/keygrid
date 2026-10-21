package qoder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// request.go 把 OpenAI chat/completions 请求体映射成 Qoder 私有 payload，
// 移植自 9router open-sse/executors/qoder.js（buildQoderRequestBody）。
//
// 与 OpenAI 兼容上游的关键差别：
//   - system 消息必须从 messages 里抽出，单独放 payload.system（上游拒绝 messages 里的 system）；
//   - 纯文本 content 扁平化成字符串；含图片时保留 image_url 块；
//   - 必须回传该模型的 model_config（错传会静默降级到别的模型）；
//   - 固定 stream:true（上游只支持 SSE，非流式由我们折叠）；
//   - chat_record_id / session_id 是由内容与账号派生的稳定标识。

// BuildInput 一次请求构造的输入。
type BuildInput struct {
	// OpenAIBody 渠道无关的 OpenAI chat/completions 请求体（pivot）。
	OpenAIBody []byte
	// ModelKey 上游模型 key（auto / ultimate / qmodel_latest …）。
	ModelKey string
	// UserID 用于派生稳定的 session_id（同账号同模型复用会话）。
	UserID string
	// Catalog 模型目录（取 model_config）。
	Catalog *Catalog
	// TierPref 上下文档位策略（空则读 QODER_CONTEXT_TIER）。
	TierPref string
	// Uploader 图片上传器（nil 时内联小图 / 占位大图）。
	Uploader Uploader
}

// Built 构造结果。
type Built struct {
	// ModelKey 归一后的上游模型 key。
	ModelKey string
	// Payload 明文字节（还需 EncodeBody 后才发）。
	Payload []byte
	// ModelConfig 本次使用的 model_config（头部 X-Model-Source 取值）。
	ModelConfig json.RawMessage
	// ModelSource model_config.source（缺省 "system"）。
	ModelSource string
}

// oaiRequest 从 OpenAI 请求体里取需要的字段。
type oaiRequest struct {
	Model               string
	Messages            []any
	Tools               any
	MaxTokens           float64
	MaxCompletionTokens float64
}

// BuildRequest 构造 Qoder 请求。
func BuildRequest(ctx context.Context, in BuildInput, rc Resolved) (*Built, error) {
	key := strings.TrimPrefix(strings.TrimSpace(in.ModelKey), "qoder/")
	if key == "" {
		return nil, fmt.Errorf("qoder: model key is empty")
	}

	req, err := parseOpenAIRequest(in.OpenAIBody)
	if err != nil {
		return nil, err
	}

	config, ok := in.Catalog.Config(key)
	if !ok || len(config) == 0 {
		return nil, fmt.Errorf("qoder: model_config for %q not yet known (fetch the model list first)", key)
	}
	var cfg map[string]any
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("qoder: parse model_config for %q: %w", key, err)
	}
	isReasoning, _ := cfg["is_reasoning"].(bool)
	modelSource := "system"
	if s, ok := cfg["source"].(string); ok && s != "" {
		modelSource = s
	}

	// 附件：上传图片、占位超大文件（失败不阻断，退化为内联/占位）。
	messages := RewriteAttachments(ctx, req.Messages, rc, in.Uploader)
	if payloadSize(messages) > MaxPayloadBytes {
		StripRemainingDataURIs(messages)
	}

	messages, systemText := normalizeMessages(messages)

	maxTokens := 32768
	if v := jsonInt(cfg["max_output_tokens"]); v > 0 {
		maxTokens = v
	}
	if req.MaxTokens > 0 && int(req.MaxTokens) < maxTokens {
		maxTokens = int(req.MaxTokens)
	}
	if req.MaxCompletionTokens > 0 && int(req.MaxCompletionTokens) < maxTokens {
		maxTokens = int(req.MaxCompletionTokens)
	}

	lastUser := lastUserText(messages)
	sessionID := stableHash("qoder-session", in.UserID, key)
	recordID := stableChatRecordID(key, messages, req.Tools, maxTokens)

	tierChoice := ResolveTier(config, PromptInput{System: systemText, Messages: messages, Tools: req.Tools}, tierPreference(in.TierPref))

	tools := []any{}
	if list, ok := req.Tools.([]any); ok {
		tools = list
	}

	payload := map[string]any{
		"request_id":       UUID(),
		"request_set_id":   recordID,
		"chat_record_id":   recordID,
		"session_id":       sessionID,
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"code_language":    "",
		"chat_prompt":      "",
		"image_urls":       nil,
		"aliyun_user_type": "",
		"system":           systemText,
		"messages":         messages,
		"tools":            tools,
		"parameters":       map[string]any{"max_tokens": maxTokens},
		"model_config":     cfg,
		"chat_context":     chatContext(lastUser, key, isReasoning),
		"business":         businessBlock(lastUser),
	}
	if tierChoice != nil {
		ApplyTier(payload, tierChoice.Tier)
	}

	raw, err := jsonMarshal(payload)
	if err != nil {
		return nil, fmt.Errorf("qoder: marshal payload: %w", err)
	}
	return &Built{ModelKey: key, Payload: raw, ModelConfig: config, ModelSource: modelSource}, nil
}

// chatContext payload.chat_context（IDE 形态的上下文块）。
func chatContext(lastUser, key string, isReasoning bool) map[string]any {
	return map[string]any{
		"chatPrompt": "",
		"imageUrls":  nil,
		"extra": map[string]any{
			"context":         []any{},
			"modelConfig":     map[string]any{"key": key, "is_reasoning": isReasoning},
			"originalContent": lastUser,
		},
		"features": []any{},
		"text":     lastUser,
	}
}

// businessBlock payload.business（客户端指纹 + 会话标题）。
func businessBlock(lastUser string) map[string]any {
	return map[string]any{
		"product":  "cli",
		"version":  "1.0.0",
		"type":     "agent",
		"stage":    "start",
		"id":       UUID(),
		"name":     truncateRunes(lastUser, 30),
		"begin_at": nowUnixMillis(),
	}
}

// parseOpenAIRequest 解析 OpenAI 请求体为构造用的中间结构。
func parseOpenAIRequest(body []byte) (oaiRequest, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return oaiRequest{}, fmt.Errorf("qoder: parse request body: %w", err)
	}
	out := oaiRequest{}
	if s, ok := m["model"].(string); ok {
		out.Model = s
	}
	if list, ok := m["messages"].([]any); ok {
		out.Messages = list
	}
	out.Tools = m["tools"]
	if v, ok := m["max_tokens"].(float64); ok {
		out.MaxTokens = v
	}
	if v, ok := m["max_completion_tokens"].(float64); ok {
		out.MaxCompletionTokens = v
	}
	return out, nil
}

// normalizeMessages 抽出 system 文本并规范化每条消息的 content。
// 返回 (messages, systemText)。
func normalizeMessages(messages []any) ([]any, string) {
	out := make([]any, 0, len(messages))
	systemParts := []string{}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" {
			if text := extractText(msg["content"]); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		clone := make(map[string]any, len(msg))
		for k, v := range msg {
			clone[k] = v
		}
		clone["content"] = normalizeContent(msg["content"])
		out = append(out, clone)
	}
	return out, strings.Join(systemParts, "\n\n")
}

// normalizeContent 规范化一条消息的 content：
// 纯文本扁平化成字符串；含图片时保留数组（图片用 OpenAI image_url 形状）。
func normalizeContent(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case nil:
		return ""
	case []any:
		blocks := []any{}
		textParts := []string{}
		hasImage := false
		pushText := func(text string) {
			if text == "" {
				return
			}
			if hasImage || len(blocks) > 0 {
				blocks = append(blocks, textBlock(text))
				return
			}
			textParts = append(textParts, text)
		}
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "image_url":
				var url string
				switch iv := item["image_url"].(type) {
				case string:
					url = iv
				case map[string]any:
					url, _ = iv["url"].(string)
				}
				if url != "" {
					blocks = append(blocks, imageURLBlock(url))
					hasImage = true
					continue
				}
			case "image":
				src, _ := item["source"].(map[string]any)
				if src != nil {
					url := ""
					if src["type"] == "base64" {
						if data, ok := src["data"].(string); ok && data != "" {
							mime, _ := src["media_type"].(string)
							if mime == "" {
								mime = "image/png"
							}
							url = "data:" + mime + ";base64," + data
						}
					} else if u, ok := src["url"].(string); ok {
						url = u
					}
					if url != "" {
						blocks = append(blocks, imageURLBlock(url))
						hasImage = true
						continue
					}
				}
			case "file":
				name := "file"
				if file, ok := item["file"].(map[string]any); ok {
					if s, ok := file["filename"].(string); ok && s != "" {
						name = s
					} else if s, ok := file["name"].(string); ok && s != "" {
						name = s
					}
				}
				pushText("[file omitted: " + name + " — Qoder reads documents via its file API, not inlined bytes]")
			case "document":
				name := "document"
				if s, ok := item["title"].(string); ok && s != "" {
					name = s
				}
				pushText("[file omitted: " + name + " — Qoder reads documents via its file API, not inlined bytes]")
			default:
				if text, ok := item["text"].(string); ok && text != "" {
					pushText(text)
				}
			}
		}
		if !hasImage {
			return strings.Join(textParts, "\n")
		}
		if len(textParts) > 0 {
			blocks = append([]any{textBlock(strings.Join(textParts, "\n"))}, blocks...)
		}
		return blocks
	}
	return fmt.Sprint(content)
}

// extractText 从 content 里抽纯文本（system 抽取用）。
func extractText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case nil:
		return ""
	case []any:
		parts := []string{}
		for _, raw := range v {
			if item, ok := raw.(map[string]any); ok {
				if s, ok := item["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprint(content)
}

// lastUserText 最后一条 user 消息的文本（会话标题 / chat_context 用）。
func lastUserText(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		if s, ok := msg["content"].(string); ok {
			return s
		}
		return extractText(msg["content"])
	}
	return ""
}

// stableHash 稳定短哈希（session_id 用）。
func stableHash(prefix string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(prefix))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// stableChatRecordID 内容派生的 chat_record_id：
// 同模型 + 同消息 + 同工具 + 同 max_tokens 稳定，便于上游复用/对账。
func stableChatRecordID(model string, messages []any, tools any, maxTokens int) string {
	h := sha256.New()
	h.Write([]byte("qoder-record"))
	h.Write([]byte{0})
	h.Write([]byte(model))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, ok := msg["role"].(string); ok && role != "" {
			h.Write([]byte{0})
			h.Write([]byte(role))
		}
		switch content := msg["content"].(type) {
		case string:
			if content != "" {
				h.Write([]byte{0})
				h.Write([]byte(content))
			}
		case []any:
			h.Write([]byte{0})
			if raw, err := jsonMarshal(content); err == nil {
				h.Write(raw)
			}
		}
	}
	if tools != nil {
		h.Write([]byte{0})
		if raw, err := jsonMarshal(tools); err == nil {
			h.Write(raw)
		}
	}
	h.Write([]byte{0})
	h.Write([]byte(fmt.Sprintf("mt=%d", maxTokens)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// jsonInt JSON 数字 → int（非法返回 0）。
func jsonInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}
