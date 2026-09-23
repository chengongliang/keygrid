package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
)

// xai.go xAI/Grok OAuth 渠道：Grok Build 使用 Responses API，默认 endpoint
// 为 cli-chat-proxy.grok.com/v1/responses，并要求一组 Grok CLI 身份请求头。
const (
	xaiTokenAuthHeader     = "X-XAI-Token-Auth"
	xaiTokenAuthValue      = "xai-grok-cli"
	xaiClientVersionHeader = "x-grok-client-version"
	xaiClientVersion       = "0.2.120"
	xaiClientIDHeader      = "x-grok-client-identifier"
	xaiClientIDValue       = "grok-shell"
	xaiAuthResponseHeader  = "x-authenticateresponse"
	xaiAuthResponseValue   = "authenticate-response"
	xaiConversationHeader  = "x-grok-conv-id"
)

// IsXAIProvider 判断是否为 xAI Grok OAuth 渠道。
func IsXAIProvider(p *model.Provider) bool {
	return p != nil && p.Kind == "oauth" && p.OAuthProvider == "xai"
}

// IsResponsesProvider 判断是否需要走 Responses 上游。Codex 与 Grok Build
// 都不是普通 /v1/chat/completions endpoint。
func IsResponsesProvider(p *model.Provider) bool {
	return IsCodexProvider(p) || IsXAIProvider(p)
}

// NormalizeXAIBaseURL 把 xAI 渠道地址归一化为完整 Responses endpoint。
// 预设使用 .../v1/responses；手工填写 .../v1 或 https://api.x.ai 也可正常工作。
func NormalizeXAIBaseURL(baseURL string) string {
	t := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if t == "" {
		return ""
	}
	if strings.HasSuffix(t, "/responses") {
		return t
	}
	if strings.HasSuffix(t, "/v1") {
		return t + "/responses"
	}
	if u, err := url.Parse(t); err == nil && u.Path == "" {
		return t + "/v1/responses"
	}
	return t + "/responses"
}

// XAIUpstreamHeaders 构建 xAI Responses 请求头。Grok Build 身份头只对官方
// cli-chat-proxy endpoint 生效，避免用户填写自定义兼容 endpoint 时收到无关头。
func XAIUpstreamHeaders(token, target, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "text/event-stream")
	h.Set("Connection", "Keep-Alive")
	if sessionID != "" {
		h.Set(xaiConversationHeader, sessionID)
	}
	if isXAIChatProxyEndpoint(target) {
		h.Set(xaiTokenAuthHeader, xaiTokenAuthValue)
		h.Set(xaiClientVersionHeader, xaiClientVersion)
		h.Set(xaiClientIDHeader, xaiClientIDValue)
		h.Set(xaiAuthResponseHeader, xaiAuthResponseValue)
		h.Set("User-Agent", "xai-grok-workspace/"+xaiClientVersion)
	}
	return h
}

func isXAIChatProxyEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && strings.EqualFold(u.Hostname(), "cli-chat-proxy.grok.com")
}

// BuildXAIRequest 把 OpenAI chat/completions 请求转换为 xAI Responses 请求。
// 基础消息/工具映射复用经过测试的 Responses 转换，再恢复 xAI 支持的采样参数。
func BuildXAIRequest(original []byte, upModel string) ([]byte, error) {
	converted, err := BuildCodexRequest(original, upModel)
	if err != nil {
		return nil, fmt.Errorf("xai: %w", err)
	}
	return normalizeXAIRequest(converted, original, upModel), nil
}

// BuildXAIRequestFromResponses 保留 Responses 入口的 input/reasoning/tools 结构，
// 只做模型替换、无状态网关要求和 xAI 支持字段恢复。
func BuildXAIRequestFromResponses(original []byte, upModel string) ([]byte, error) {
	converted, err := BuildCodexRequestFromResponses(original, upModel)
	if err != nil {
		return nil, fmt.Errorf("xai: %w", err)
	}
	return normalizeXAIRequest(converted, original, upModel), nil
}

func normalizeXAIRequest(converted, original []byte, upModel string) []byte {
	var out, src map[string]any
	if json.Unmarshal(converted, &out) != nil || json.Unmarshal(original, &src) != nil {
		return converted
	}
	// xAI executor 不为普通 Grok 请求注入 Codex 专用默认 instructions。
	if _, exists := src["instructions"]; !exists && out["instructions"] == codexDefaultInstructions {
		delete(out, "instructions")
	}
	// BuildCodexRequest 的 allowlist偏向 ChatGPT backend；xAI Responses 还支持
	// 这些标准字段，恢复前端传入值。stop 在 xAI Responses 中不受支持。
	for _, key := range []string{
		"temperature", "top_p", "top_k", "max_output_tokens", "parallel_tool_calls",
		"service_tier", "text", "metadata", "truncation", "include",
	} {
		if value, ok := src[key]; ok {
			out[key] = value
		}
	}
	if _, ok := out["max_output_tokens"]; !ok {
		if value, ok := src["max_completion_tokens"]; ok {
			out["max_output_tokens"] = value
		} else if value, ok := src["max_tokens"]; ok {
			out["max_output_tokens"] = value
		}
	}
	delete(out, "stop")
	out["model"] = upModel
	out["stream"] = true
	out["store"] = false
	encoded, err := json.Marshal(out)
	if err != nil {
		return converted
	}
	return encoded
}

// resolveXAISession 仅为 Grok Composer 派生稳定会话；普通 Grok 模型不额外
// 注入 prompt_cache_key，避免把无状态请求绑定到网关身份。
func resolveXAISession(r *http.Request, body []byte, userID, apiKeyID, providerID int64, upModel string) string {
	if session := explicitResponsesSession(r, body); session != "" {
		return session
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(upModel)), "grok-composer-") {
		return ""
	}
	return resolveCodexSession(r, body, userID, apiKeyID, providerID)
}

func explicitResponsesSession(r *http.Request, body []byte) string {
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
	return ""
}

// upstreamCallXAI 执行 xAI Responses 调用。Responses SSE 的聚合与三入口桥接
// 与 Codex 共用，差异仅在请求头和 xAI endpoint。
func (h *Handler) upstreamCallXAI(
	ctx context.Context,
	client *http.Client,
	target string,
	token string,
	body []byte,
	clientStream bool,
	entry string,
	w http.ResponseWriter,
	ua string,
) (int, UsageRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return 0, UsageRecord{}, err
	}
	for k, vs := range XAIUpstreamHeaders(token, target, codexSessionFromBody(body)) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	upResp, err := client.Do(req)
	if err != nil {
		return 0, UsageRecord{}, err
	}
	defer upResp.Body.Close()
	if upResp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(upResp.Body, 4<<10))
		return upResp.StatusCode, UsageRecord{}, &UpstreamError{Status: upResp.StatusCode, Body: string(b), ContentType: upResp.Header.Get("Content-Type")}
	}
	if status, err := peekCodexSSEError(upResp); err != nil {
		return status, UsageRecord{}, err
	}

	if clientStream {
		switch entry {
		case protoResponses:
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
		return upResp.StatusCode, UsageRecord{}, fmt.Errorf("xai: %s", agg.ErrMsg)
	}
	usage := UsageRecord{PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens, Found: agg.PromptTokens > 0 || agg.CompletionTokens > 0}
	writeEntryJSON(w, entry, upModelFromBody(body), *agg)
	return upResp.StatusCode, usage, nil
}
