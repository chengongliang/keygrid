package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
	"github.com/chengongliang/keygrid/internal/ssrf"
)

// providermodels.go 上游模型探测：
// - FetchUpstreamModels POST /api/providers/{id}/models    —— 按已存渠道拉取上游 /v1/models
// - TestModel           POST /api/providers/{id}/test_model —— 对指定模型发最小请求验证可用性
// - ProbeModels         POST /api/providers/probe_models    —— 配置时探测：未落库的 base_url+api_key
//   或已有渠道（provider_id，用存储凭据）均可拉取，供前端「勾选启用模型」使用。
// 均为只读探测：不写库、不触发 oauth 兜底刷新（过期 token 由上游 401 反映），
// 统一按 OpenAI 兼容风格请求上游（relay 转发路径同此约定）。

// fetchModelsResult 上游模型拉取结果。
type fetchModelsResult struct {
	OK      bool     `json:"ok"`
	Status  int      `json:"status"`
	Models  []string `json:"models,omitempty"`
	Error   string   `json:"error,omitempty"`
	Channel string   `json:"channel_name"`
}

// fetchAndParseUpstreamModels 拉取上游 /v1/models 并解析模型 ID 列表。
// client 由调用方按渠道代理开关构建（probeClient）。
// 返回 (models, status, errMsg)：errMsg 非空表示失败（status=0 为网络错误）。
func fetchAndParseUpstreamModels(ctx context.Context, client *http.Client, baseURL, secret string) ([]string, int, string) {
	status, body := fetchUpstreamModels(ctx, client, baseURL, secret)
	switch {
	case status == 0:
		return nil, status, body
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, status, "upstream rejected credential (" + truncateBody(body, 256) + ")"
	case status != http.StatusOK:
		return nil, status, "upstream returned " + itoa(status) + ": " + truncateBody(body, 256)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, status, "parse /v1/models response failed: " + err.Error()
	}
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, status, ""
}

// FetchUpstreamModels POST /api/providers/{id}/models —— 已存渠道的上游模型列表。
func (h *TestHandler) FetchUpstreamModels(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.Op.GetProvider(userID, id)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}

	res := fetchModelsResult{Channel: p.Name}
	emit := func() { resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res}) }

	// Codex 上游无 /v1/models：返回动态模型目录（远程刷新的 Codex 客户端目录，
	// 内嵌快照兑底），目录异常时回退预设模型目录（wizard 预填的同一份）
	if relay.IsResponsesProvider(p) {
		models := responsesProviderModels(p)
		if len(models) > 0 {
			res.OK = true
			res.Models = models
			emit()
			return
		}
	}

	secret, _, errMsg := h.probeSecret(p)
	if errMsg != "" {
		res.Error = errMsg
		emit()
		return
	}

	client, err := probeClient(h.Op, p.UseProxy, 30*time.Second)
	if err != nil {
		res.Error = err.Error()
		emit()
		return
	}

	models, status, errMsg := fetchAndParseUpstreamModels(r.Context(), client, p.BaseURL, secret)
	res.Status = status
	if errMsg != "" {
		res.Error = errMsg
	} else {
		res.OK = true
		res.Models = models
	}
	emit()
}

// probeModelsReq POST /api/providers/probe_models 请求体。
// api_key 与 provider_id 二选一：新增场景传 base_url+api_key（渠道还没落库）；
// 编辑场景传 provider_id（用存储凭据，base_url 可覆盖为表单里的新值）。
// use_proxy：新增场景前端向导的勾选状态（渠道未落库，代理开关随请求传入）；
// 编辑场景以落库渠道的 use_proxy 为准（该字段传入无效）。
type probeModelsReq struct {
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	ProviderID int64  `json:"provider_id"`
	UseProxy   bool   `json:"use_proxy"`
}

// ProbeModels POST /api/providers/probe_models —— 配置时探测上游模型（不要求渠道已存在）。
func (h *TestHandler) ProbeModels(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	res := fetchModelsResult{}
	emit := func() { resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res}) }

	var req probeModelsReq
	if err := resp.Decode(r, &req); err != nil {
		res.Error = "invalid json body"
		emit()
		return
	}

	// provider_id：加载用户自己的渠道（用于存储凭据 / 默认 base_url）
	var p *model.Provider
	if req.ProviderID > 0 {
		loaded, err := h.Op.GetProvider(userID, req.ProviderID)
		if err != nil {
			resp.NotFound(w, "provider not found")
			return
		}
		p = loaded
		res.Channel = p.Name

		// Codex 上游无 /v1/models（base_url 即完整 endpoint，拼 /v1/models 会被
		// 网关 403 拒绝）：返回动态模型目录（远程刷新，内嵌快照兑底），与
		// FetchUpstreamModels 同策略；目录异常时回退预设
		if relay.IsResponsesProvider(p) {
			models := responsesProviderModels(p)
			if len(models) > 0 {
				res.OK = true
				res.Models = models
				emit()
				return
			}
		}
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" && p != nil {
		baseURL = p.BaseURL
	}
	if baseURL == "" {
		res.Error = "base_url or provider_id required"
		emit()
		return
	}
	if err := ssrf.CheckBaseURL(baseURL); err != nil {
		res.Error = err.Error()
		emit()
		return
	}

	// 凭据：显式 api_key 优先，否则用存储凭据
	secret := strings.TrimSpace(req.APIKey)
	if secret == "" {
		if p == nil {
			res.Error = "api_key or provider_id required"
			emit()
			return
		}
		s, _, errMsg := h.probeCredential(p)
		if errMsg != "" {
			res.Error = errMsg
			emit()
			return
		}
		secret = s
	}

	// 代理开关：已有渠道用落库值；新增场景用请求体携带的向导勾选状态
	useProxy := req.UseProxy
	if p != nil {
		useProxy = p.UseProxy
	}
	client, err := probeClient(h.Op, useProxy, 30*time.Second)
	if err != nil {
		res.Error = err.Error()
		emit()
		return
	}

	models, status, errMsg := fetchAndParseUpstreamModels(r.Context(), client, baseURL, secret)
	res.Status = status
	if errMsg != "" {
		res.Error = errMsg
	} else {
		res.OK = true
		res.Models = models
	}
	emit()
}

// responsesProviderModels Responses 渠道没有通用 /v1/models：Codex 使用动态
// 目录，xAI OAuth 使用 Grok Build 预设目录作为稳定兜底。
func responsesProviderModels(p *model.Provider) []string {
	if p == nil {
		return nil
	}
	if relay.IsCodexProvider(p) {
		if models := oauth.CodexModelCatalog(p.OAuthProvider); len(models) > 0 {
			return models
		}
	}
	if preset, ok := oauth.LookupPreset(p.OAuthProvider); ok {
		return preset.Models
	}
	return nil
}

// testModelReq POST /api/providers/{id}/test_model 请求体。
// 仅传 model 时为轻量测活（prompt="ping"、max_tokens=1）；
// 传 prompt/max_tokens/stream 时为模拟真实请求测试（贴近实际调用链路）。
type testModelReq struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
}

// testModelResult 单模型可用性测试结果（含真实请求详情）。
type testModelResult struct {
	OK               bool   `json:"ok"`
	Status           int    `json:"status"`
	LatencyMs        int64  `json:"latency_ms"`
	FirstTokenMs     int64  `json:"first_token_ms,omitempty"` // 流式首字延迟
	Model            string `json:"model"`
	Stream           bool   `json:"stream,omitempty"`
	Prompt           string `json:"prompt,omitempty"`
	Content          string `json:"content,omitempty"`           // 模型输出内容
	ReasoningContent string `json:"reasoning_content,omitempty"` // 推理模型思考过程（content 为空时展示）
	FinishReason     string `json:"finish_reason,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	Error            string `json:"error,omitempty"`
	Channel          string `json:"channel_name"`
}

// TestModel 对指定模型发一次 chat completion。
// 轻量测活：max_tokens=1 只验证「模型存在可用」；模拟真实请求：完整读取响应，
// 返回提示词/输出内容/耗时/首字延迟/token 用量（流式时聚合 SSE delta）。
func (h *TestHandler) TestModel(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.Op.GetProvider(userID, id)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}

	res := testModelResult{Channel: p.Name}
	emit := func() { resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res}) }

	var req testModelReq
	if err := resp.Decode(r, &req); err != nil {
		res.Error = "invalid json body"
		emit()
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		res.Error = "model is required"
		emit()
		return
	}
	res.Model = strings.TrimSpace(req.Model)
	res.Stream = req.Stream

	// 轻量测活：仅 model 时取最小参数；模拟真实请求：自定义提示词与输出长度
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "ping"
	}
	if rs := []rune(prompt); len(rs) > 4000 { // 防滥用：提示词上限 4000 字符
		prompt = string(rs[:4000])
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1
	}
	if maxTokens > 2048 {
		maxTokens = 2048
	}
	res.Prompt = prompt

	secret, accountID, errMsg := h.probeSecret(p)
	if errMsg != "" {
		res.Error = errMsg
		emit()
		return
	}

	client, err := probeClient(h.Op, p.UseProxy, 120*time.Second)
	if err != nil {
		res.Error = err.Error()
		emit()
		return
	}
	pr := probeUpstreamChat(r.Context(), client, p, secret, accountID, res.Model, prompt, maxTokens, req.Stream)
	res.Status = pr.Status
	res.LatencyMs = pr.LatencyMs
	if req.Stream {
		res.FirstTokenMs = pr.FirstTokenMs
	}
	res.Content = pr.Content
	res.ReasoningContent = pr.Reasoning
	res.FinishReason = pr.FinishReason
	res.PromptTokens = pr.PromptTokens
	res.CompletionTokens = pr.CompletionTokens
	switch {
	case pr.Status == 0:
		res.Error = pr.Err
	case pr.Status == http.StatusUnauthorized || pr.Status == http.StatusForbidden:
		res.Error = "upstream rejected credential (" + pr.Err + ")"
	case pr.Status >= 400:
		res.Error = "model unavailable (HTTP " + itoa(pr.Status) + "): " + pr.Err
	default:
		res.OK = true
	}
	emit()
}

// chatProbe 真实 chat 请求探测结果。
type chatProbe struct {
	Status           int
	LatencyMs        int64
	FirstTokenMs     int64
	Content          string
	Reasoning        string // 推理模型思考过程（reasoning_content / reasoning / reasoning_text）
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
	Err              string // 网络错误或上游 4xx/5xx 响应摘要
}

// pickReasoning 兼容各上游推理字段命名：reasoning_content（DeepSeek 系）、
// reasoning（vLLM 系）、reasoning_text。取第一个非空值。
func pickReasoning(parts ...string) string {
	for _, s := range parts {
		if s != "" {
			return s
		}
	}
	return ""
}

// buildUpstreamChatProbeReq 构建上游探测 chat 请求（TestModel/TestModelStream 共用）：
// api_key 渠道 POST {base_url}/v1/chat/completions；Codex 渠道把 chat 载荷转
// Responses 协议后直发 base_url（上游强制流式，stream 参数仅影响无意义的客户端形态）。
func buildUpstreamChatProbeReq(ctx context.Context, p *model.Provider, secret, accountID, model, prompt string, maxTokens int, stream bool) (*http.Request, error) {
	chatPayload, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"stream":     stream,
	})

	var target string
	var payload []byte
	if relay.IsResponsesProvider(p) {
		var converted []byte
		var err error
		if relay.IsXAIProvider(p) {
			converted, err = relay.BuildXAIRequest(chatPayload, model)
		} else {
			converted, err = relay.BuildCodexRequest(chatPayload, model)
		}
		if err != nil {
			return nil, err
		}
		if relay.IsXAIProvider(p) {
			target = relay.NormalizeXAIBaseURL(p.BaseURL)
		} else {
			target = relay.NormalizeCodexBaseURL(p.BaseURL)
		}
		payload = converted
	} else {
		target = strings.TrimRight(p.BaseURL, "/") + "/v1/chat/completions"
		payload = chatPayload
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	if relay.IsXAIProvider(p) {
		for k, vs := range relay.XAIUpstreamHeaders(secret, target, "") {
			req.Header.Set(k, vs[0])
		}
	} else if relay.IsCodexProvider(p) {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
		if accountID != "" {
			req.Header.Set("chatgpt-account-id", accountID)
		}
	}
	return req, nil
}

// probeUpstreamChat 发送一次 chat completion（支持流式），聚合输出内容与耗时信息。
// 与 probeUpstream（TestProvider 轻量探活）不同：这里读取完整响应体以还原真实对话。
// Codex 渠道：chat 请求 → Responses 协议转换后直发 base_url，SSE 聚合回读。
// client 由调用方按渠道代理开关构建（probeClient）。
func probeUpstreamChat(ctx context.Context, client *http.Client, p *model.Provider, secret, accountID, model, prompt string, maxTokens int, stream bool) chatProbe {
	req, err := buildUpstreamChatProbeReq(ctx, p, secret, accountID, model, prompt, maxTokens, stream)
	if err != nil {
		return chatProbe{Err: err.Error()}
	}

	start := time.Now()
	respUp, err := client.Do(req)
	if err != nil {
		return chatProbe{LatencyMs: time.Since(start).Milliseconds(), Err: err.Error()}
	}
	defer respUp.Body.Close()

	if respUp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(respUp.Body, 4<<10))
		return chatProbe{Status: respUp.StatusCode, LatencyMs: time.Since(start).Milliseconds(),
			Err: truncateBody(string(buf), 256)}
	}

	if relay.IsResponsesProvider(p) {
		agg, aggErr := relay.AggregateCodexStream(respUp.Body)
		latency := time.Since(start).Milliseconds()
		if aggErr != nil && agg.Content == "" {
			return chatProbe{Status: respUp.StatusCode, LatencyMs: latency, Err: aggErr.Error()}
		}
		pr := chatProbe{Status: respUp.StatusCode, LatencyMs: latency,
			Content: agg.Content, Reasoning: agg.Reasoning,
			FinishReason: agg.FinishReason,
			PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens}
		if pr.FinishReason == "" {
			pr.FinishReason = "stop"
		}
		if agg.ErrMsg != "" {
			pr.Err = agg.ErrMsg
		}
		pr.FirstTokenMs = latency
		return pr
	}

	if stream {
		return readStreamChat(respUp.Body, start)
	}
	buf, err := io.ReadAll(io.LimitReader(respUp.Body, 4<<20))
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return chatProbe{Status: respUp.StatusCode, LatencyMs: latency, Err: "read body: " + err.Error()}
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ReasoningText    string `json:"reasoning_text"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(buf, &parsed); err != nil {
		return chatProbe{Status: respUp.StatusCode, LatencyMs: latency, Err: "parse response failed: " + err.Error()}
	}
	pr := chatProbe{Status: respUp.StatusCode, LatencyMs: latency}
	if len(parsed.Choices) > 0 {
		pr.Content = parsed.Choices[0].Message.Content
		pr.Reasoning = pickReasoning(parsed.Choices[0].Message.ReasoningContent, parsed.Choices[0].Message.Reasoning, parsed.Choices[0].Message.ReasoningText)
		if parsed.Choices[0].FinishReason != nil {
			pr.FinishReason = *parsed.Choices[0].FinishReason
		}
	}
	pr.PromptTokens = parsed.Usage.PromptTokens
	pr.CompletionTokens = parsed.Usage.CompletionTokens
	return pr
}

// readStreamChat 聚合 SSE 流式响应：拼接 delta.content，统计首字延迟与 finish_reason。
func readStreamChat(body io.Reader, start time.Time) chatProbe {
	pr := chatProbe{Status: http.StatusOK}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var sb strings.Builder
	var rsb strings.Builder
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if pr.FirstTokenMs == 0 {
			pr.FirstTokenMs = time.Since(start).Milliseconds()
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ReasoningText    string `json:"reasoning_text"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 跳过注释/心跳行
		}
		if len(chunk.Choices) > 0 {
			sb.WriteString(chunk.Choices[0].Delta.Content)
			rsb.WriteString(pickReasoning(chunk.Choices[0].Delta.ReasoningContent, chunk.Choices[0].Delta.Reasoning, chunk.Choices[0].Delta.ReasoningText))
			if chunk.Choices[0].FinishReason != nil {
				pr.FinishReason = *chunk.Choices[0].FinishReason
			}
		}
		if chunk.Usage != nil {
			pr.PromptTokens = chunk.Usage.PromptTokens
			pr.CompletionTokens = chunk.Usage.CompletionTokens
		}
	}
	if err := sc.Err(); err != nil {
		pr.Err = "stream interrupted: " + err.Error()
	}
	pr.LatencyMs = time.Since(start).Milliseconds()
	pr.Content = sb.String()
	pr.Reasoning = rsb.String()
	if pr.FirstTokenMs == 0 {
		pr.FirstTokenMs = pr.LatencyMs
	}
	return pr
}

// probeSecret 已存渠道探测公共前置：SSRF 校验 + 凭据检查 + 解密上游密钥。
// 返回 errMsg 非空表示失败（已含可读原因），由调用方直接作为 res.Error 透出。
func (h *TestHandler) probeSecret(p *model.Provider) (string, string, string) {
	if err := ssrf.CheckBaseURL(p.BaseURL); err != nil {
		return "", "", err.Error()
	}
	return h.probeCredential(p)
}

// probeCredential 凭据检查 + 解密（不含 SSRF，base_url 可能被探测请求覆盖）。
func (h *TestHandler) probeCredential(p *model.Provider) (string, string, string) {
	cred, err := h.Op.GetCredentialByProviderID(p.ID)
	if err != nil {
		return "", "", "credential not configured (oauth not authorized yet?)"
	}
	if cred.Status == "revoked" {
		return "", "", "credential revoked, re-auth required"
	}
	secret, extra, err := credentialSecretForTest(cred)
	if err != nil {
		return "", "", err.Error()
	}
	return secret, extra["chatgptAccountId"], ""
}

// truncateBody 错误摘要截断（上游响应体可能很大，不整体塞进错误信息）。
func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fetchUpstreamModels GET {base_url}/v1/models，响应体上限 256KB。
// client 由调用方按渠道代理开关构建（probeClient）。
func fetchUpstreamModels(ctx context.Context, client *http.Client, baseURL, secret string) (int, string) {
	target := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+secret)

	respUp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer respUp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(respUp.Body, 256<<10))
	if err != nil {
		return respUp.StatusCode, "read body: " + err.Error()
	}
	return respUp.StatusCode, string(buf)
}
