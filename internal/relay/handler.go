package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/quota"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// Handler 转发引擎：OpenAI 兼容、SSE 流式、failover、熔断、限流、异步记账。
// 多协议入口：/v1/chat/completions（openai）、/v1/messages（anthropic）、
// /v1/responses（responses）—— 三个入口共享路由/熔断/记账骨架（relayCore），
// 以 openai chat/completions 为 pivot 双向转换（anthropic.go / responses_api.go /
// stream_bridge.go / codex.go）。
type Handler struct {
	Op         *op.Op
	HTTPClient *http.Client
	Breaker    *Breaker
	Limiter    *RateLimiter
	Usage      *UsageWriter
	// ErrSink 失败诊断异步写入器（nil = 同步落库）
	ErrSink *ErrorWriter
	// Pricing 模型价格缓存（计费；nil = 不计费，cost=0）
	Pricing *Pricing
	// Quota key 级额度硬限额（计费；nil = 不拦截）
	Quota *QuotaEnforcer
	// OAuth 调度器（nil = oauth 渠道兜底刷新不可用）
	OAuth     *oauth.Scheduler
	OAuthLead time.Duration
	// QuotaSyncer 额度同步器（nil = 不做 x-codex-* 响应头被动观察）
	QuotaSyncer *quota.Syncer

	// 测试注入用
	Now func() time.Time
}

// relayRequest 入口解析结果：model 用于路由/记账，pivot 是渠道无关的中间请求体。
// Raw 保留原始请求体（仅 responses 入口填充）：Codex 渠道同为 Responses 形状，
// 走直转避免 pivot 双重有损转换（丢 local_shell 工具 / reasoning item）。
type relayRequest struct {
	Model  string
	Stream bool
	Pivot  []byte // openai chat/completions 形状
	Raw    []byte // 原始入口请求体（responses 入口专用，其余入口为 nil）
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

func NewHandler(o *op.Op) *Handler {
	return &Handler{
		Op:         o,
		HTTPClient: newUpstreamClient(upstreamTimeout),
		Breaker:    NewBreaker(),
	}
}

// SetRateLimiter 注入限流器（Redis 就绪后调用；nil = 不限流）。
func (h *Handler) SetRateLimiter(rl *RateLimiter) { h.Limiter = rl }

// SetUsageWriter 注入异步记账器（nil 则退化为同步写）。
func (h *Handler) SetUsageWriter(uw *UsageWriter) { h.Usage = uw }

// SetErrorWriter 注入失败诊断异步写入器（nil 则退化为同步写）。
func (h *Handler) SetErrorWriter(ew *ErrorWriter) { h.ErrSink = ew }

// SetPricing 注入价格缓存（计费；nil = 不计费）。
func (h *Handler) SetPricing(p *Pricing) { h.Pricing = p }

// SetQuotaEnforcer 注入额度检查器（计费；nil = 不拦截）。
func (h *Handler) SetQuotaEnforcer(q *QuotaEnforcer) { h.Quota = q }

// SetOAuthScheduler 注入 OAuth 调度器（nil = oauth 渠道兜底刷新不可用）。
func (h *Handler) SetOAuthScheduler(s *oauth.Scheduler, lead time.Duration) {
	h.OAuth = s
	h.OAuthLead = lead
}

// SetQuotaSyncer 注入额度同步器（x-codex-* 响应头被动观察；nil = 不观察）。
func (h *Handler) SetQuotaSyncer(s *quota.Syncer) { h.QuotaSyncer = s }

// ---- 入口 handlers ----

// ChatCompletions POST /v1/chat/completions（OpenAI 兼容入口）。
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	h.relayEntry(w, r, protoOpenAI)
}

// Messages POST /v1/messages（Anthropic Messages 兼容入口）。
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	h.relayEntry(w, r, protoAnthropic)
}

// Responses POST /v1/responses（OpenAI Responses API 入口）。
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	h.relayEntry(w, r, protoResponses)
}

// relayEntry 入口骨架：读 body → 按入口协议解析成 pivot → 公共转发流程。
func (h *Handler) relayEntry(w http.ResponseWriter, r *http.Request, entry string) {
	start := h.now()

	// 维护模式 —— /v1 转发面整体拒绝（管理端 API 不受影响）
	if h.Op != nil {
		if mm, err := h.Op.GetMaintenanceMode(); err == nil && mm {
			maintText, _ := h.Op.GetSetting("announcement")
			gatewayError(w, entry, http.StatusServiceUnavailable,
				"platform under maintenance"+maintSuffix(maintText))
			return
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		gatewayError(w, entry, http.StatusBadRequest, "read body failed")
		return
	}

	var req relayRequest
	switch entry {
	case protoAnthropic:
		pivot, modelName, stream, perr := anthropicToPivotRequest(body)
		if perr != nil {
			gatewayError(w, entry, http.StatusBadRequest, perr.Error())
			return
		}
		req = relayRequest{Model: modelName, Stream: stream, Pivot: pivot}
	case protoResponses:
		pivot, modelName, stream, perr := responsesToPivotRequest(body)
		if perr != nil {
			gatewayError(w, entry, http.StatusBadRequest, perr.Error())
			return
		}
		req = relayRequest{Model: modelName, Stream: stream, Pivot: pivot, Raw: body}
	default: // openai chat/completions
		var cr chatRequest
		if err := json.Unmarshal(body, &cr); err != nil {
			gatewayError(w, entry, http.StatusBadRequest, "invalid json body")
			return
		}
		if cr.Model == "" {
			gatewayError(w, entry, http.StatusBadRequest, "model required")
			return
		}
		req = relayRequest{Model: cr.Model, Stream: cr.Stream, Pivot: body}
	}

	h.relayCore(w, r, entry, req, start)
}

// relayCore 公共转发流程：模型限制 → 限流 → 候选渠道 → failover 循环。
// pivot 已按入口协议解析好；每个渠道按自身协议（channelProto）生成上游请求、
// 调用并把响应回转成入口格式。
func (h *Handler) relayCore(w http.ResponseWriter, r *http.Request, entry string, req relayRequest, start time.Time) {
	userID := middleware.UserID(r.Context())
	apiKeyID := middleware.APIKeyID(r.Context())
	// 客户端 UA：默认策略下透传给上游（渠道可改为固定/自定义，见 ua.go）
	clientUA := r.Header.Get("User-Agent")

	// key 级限制：模型白名单（ModelLimit）+ 渠道白名单（ProviderLimit），二者均空 = 不限
	apiKeyObj := middleware.APIKeyObj(r.Context())
	if apiKeyObj != nil && apiKeyObj.ModelLimit != "" && !middleware.ModelAllowed(apiKeyObj.ModelLimit, req.Model) {
		gatewayError(w, entry, http.StatusForbidden, "model "+req.Model+" not allowed for this api key")
		return
	}
	var allowedProviders []int64
	if apiKeyObj != nil {
		allowedProviders = apiKeyObj.ProviderIDs()
	}

	// 0. 限流（按 api_key：RPM + 并发）
	release, ok := h.Limiter.Check(r.Context(), apiKeyID)
	if !ok {
		rateLimitResponse(w)
		return
	}
	defer release()

	// 0.5 额度硬限额（key 级，计费）：超限 402，请求不进渠道池
	if apiKeyObj != nil && h.Quota != nil {
		if used, limit, over := h.Quota.OverLimit(r.Context(), apiKeyObj); over {
			quotaExceededResponse(w, entry, used, limit)
			return
		}
	}

	// 1. 在 user 自己的渠道池里构建候选（priority 降序 + 同级随机；key 渠道白名单过滤）
	ps, err := h.Op.ListProviders(userID)
	if err != nil {
		h.noteFailure(userID, apiKeyID, 0, req.Model, errKindNoChannel, 0, "list providers failed: "+err.Error(), start)
		gatewayError(w, entry, http.StatusServiceUnavailable, "no available channel for model "+req.Model)
		return
	}
	cands := buildCandidates(ps, req.Model, allowedProviders)
	if len(cands) == 0 {
		// 绑定了渠道但白名单内没有可用候选：403 比 503 更能提示是 key 绑定导致
		// （渠道被禁用/删除，或绑定渠道均不支持该模型）
		if len(allowedProviders) > 0 {
			h.noteFailure(userID, apiKeyID, 0, req.Model, errKindNoChannel, http.StatusForbidden,
				"no bound channel of this api key serves model "+req.Model, start)
			gatewayError(w, entry, http.StatusForbidden, "no bound channel of this api key serves model "+req.Model)
			return
		}
		h.noteFailure(userID, apiKeyID, 0, req.Model, errKindNoChannel, http.StatusServiceUnavailable,
			"no available channel for model "+req.Model+" (no channel serves it, or all are disabled)", start)
		gatewayError(w, entry, http.StatusServiceUnavailable, "no available channel for model "+req.Model)
		return
	}

	// 2. 依序尝试渠道：熔断检查 → 解密凭据 → 调上游；失败 failover
	var lastStatus int
	var lastErrMsg string
	// 上游正文摘要仅回传客户端，不写日志（Error() 本身已不含正文，见 UpstreamError）
	var lastErrDetail string
	// 一次请求内同一渠道最多计一次熔断失败：failover 会把同一个请求打到多条渠道，
	// 去重避免重复计数把单个坏请求放大成多条渠道的熔断
	failedChannels := map[int64]bool{}
	breakerFail := func(p *model.Provider) {
		if !breakerCheckEnabled(p) {
			return // 未开启熔断检测：只 failover，不累计任何失败
		}
		if failedChannels[p.ID] {
			return
		}
		failedChannels[p.ID] = true
		h.Breaker.OnFailure(p.ID)
	}
	for _, cand := range cands {
		// 熔断检测按渠道开关（默认关）：未开启时既不做放行检查、也不消耗探测租约，
		// 未开启熔断检测的渠道永远放行。
		if breakerCheckEnabled(cand.provider) {
			allow, probe := h.Breaker.Allow(cand.provider.ID)
			if probe != 0 {
				// half-open 探测租约：本次渠道尝试无论是成功、失败、客户端断开、
				// 凭据失效还是 continue 到下一候选，都要归还探测资格。漏掉归还时
				// 该渠道会一直卡在 half-open（Allow 恒为 false），后续请求全部
				// “circuit open” —— 直到租约（ProbeTimeout）超时自动蒸发。
				defer h.Breaker.ReleaseOne(cand.provider.ID, probe)
			}
			if !allow {
				lastErrMsg = "channel " + cand.provider.Name + " circuit open"
				h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindCircuitOpen, 0,
					"circuit open: tripped by consecutive failures (reset it from the channel health view)", start)
				continue
			}
		}

		cred, err := h.Op.GetCredentialByProviderID(cand.provider.ID)
		if err != nil {
			breakerFail(cand.provider)
			lastErrMsg = "credential missing for " + cand.provider.Name
			h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindCredential, 0, lastErrMsg, start)
			continue
		}
		apiKey, tokenExtra, credStatus, err := h.credentialSecret(r.Context(), cred, cand.provider)
		if err != nil {
			breakerFail(cand.provider)
			lastErrMsg = "invalid credential for " + cand.provider.Name + ": " + err.Error()
			h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindCredential, 0, lastErrMsg, start)
			continue
		}
		if credStatus == "revoked" {
			lastErrMsg = "channel " + cand.provider.Name + " credential revoked (re-auth required)"
			h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindCredential, 0, lastErrMsg, start)
			continue
		}

		proto := channelProto(cand.provider)
		var upstreamBody []byte
		var target string
		switch proto {
		case protoResponses:
			// Codex 与 xAI 都使用 Responses 上游，但请求归一化、会话头和
			// endpoint 规则不同，分别处理。
			src := req.Raw
			if IsXAIProvider(cand.provider) {
				if len(src) > 0 {
					upstreamBody, err = BuildXAIRequestFromResponses(src, cand.upModel)
				} else {
					upstreamBody, err = BuildXAIRequest(req.Pivot, cand.upModel)
				}
				if err == nil {
					sessionSource := src
					if len(sessionSource) == 0 {
						sessionSource = req.Pivot
					}
					sessionID := resolveXAISession(r, sessionSource, userID, apiKeyID, cand.provider.ID, cand.upModel)
					if sessionID != "" {
						upstreamBody, err = injectCodexSession(upstreamBody, sessionID)
					}
				}
				target = NormalizeXAIBaseURL(cand.provider.BaseURL)
			} else {
				// Codex 渠道：base_url 即 Responses endpoint。原始 responses 请求直转
				// （完整保留 local_shell 等工具与 reasoning item）；无原始体（旧调用方）
				// 时回退 pivot 双转。
				if len(src) > 0 {
					upstreamBody, err = BuildCodexRequestFromResponses(src, cand.upModel)
				} else {
					upstreamBody, err = BuildCodexRequest(req.Pivot, cand.upModel)
				}
				if err == nil {
					sessionSource := src
					if len(sessionSource) == 0 {
						sessionSource = req.Pivot
					}
					sessionID := resolveCodexSession(r, sessionSource, userID, apiKeyID, cand.provider.ID)
					upstreamBody, err = injectCodexSession(upstreamBody, sessionID)
				}
				target = NormalizeCodexBaseURL(cand.provider.BaseURL)
			}
			if err != nil {
				breakerFail(cand.provider)
				lastErrMsg = "responses transform failed for " + cand.provider.Name + ": " + err.Error()
				h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindTransform, 0, lastErrMsg, start)
				continue
			}
		case protoAnthropic:
			upstreamBody, err = pivotToAnthropicRequest(req.Pivot, cand.upModel)
			if err != nil {
				breakerFail(cand.provider)
				lastErrMsg = "anthropic transform failed for " + cand.provider.Name + ": " + err.Error()
				h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindTransform, 0, lastErrMsg, start)
				continue
			}
			target = upstreamTargetAnthropic(cand.provider.BaseURL)
		default:
			upstreamBody = buildUpstreamBody(req.Pivot, req.Model, cand.upModel)
			target = upstreamTarget(cand.provider.BaseURL)
		}

		// 渠道级出口代理：勾选代理但平台未配置 → failover 该渠道（不静默直连）
		client, perr := h.clientFor(cand.provider)
		if perr != nil {
			breakerFail(cand.provider)
			lastErrMsg = perr.Error()
			h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindProxy, 0, lastErrMsg, start)
			continue
		}

		// 上游 UA：默认透传客户端（Codex 渠道保持固定值），渠道可自定义/强制透传
		ua := ResolveUserAgent(cand.provider, proto, clientUA)

		var status int
		var usage UsageRecord
		var callErr error
		// 仅经过 pivot 的 Responses 请求需要还原工具；每次重试使用独立状态。
		responseWriter := w
		if entry == protoResponses && proto != protoResponses {
			responseWriter = bridgeResponsesTools(w, req.Raw)
		}
		switch proto {
		case protoResponses:
			if IsXAIProvider(cand.provider) {
				status, usage, callErr = h.upstreamCallXAI(r.Context(), client, target, apiKey,
					upstreamBody, req.Stream, entry, w, ua)
			} else {
				status, usage, callErr = h.upstreamCallCodex(r.Context(), client, target, apiKey,
					tokenExtra["chatgptAccountId"], upstreamBody, req.Stream, entry, w, cand.provider, ua)
			}
		case protoAnthropic:
			status, usage, callErr = h.upstreamCallAnthropic(r.Context(), client, target, cand.provider,
				apiKey, upstreamBody, req.Stream, entry, responseWriter, ua)
		default:
			status, usage, callErr = h.upstreamCall(r.Context(), client, target, apiKey,
				upstreamBody, req.Stream, entry, responseWriter, ua)
		}

		// 记账（含失败请求：tokens=0，status/latency 保留）
		h.recordUsage(r.Context(), userID, apiKeyID, cand.provider.ID, cand.provider, req.Model, usage, status, time.Since(start))

		if callErr != nil && callErr != errClientGone {
			// 客户端取消会让上游 Body.Read 返回 context canceled，而不一定被
			// 转换成 errClientGone。必须先判断再记录渠道失败，否则 Codex 的
			// 工具轮次切换/重试会在几次取消后把健康渠道熔断。
			if clientGone(r.Context().Err()) || clientGone(callErr) {
				return
			}
			// 只有渠道/网络层面的故障才计入熔断：4xx（参数错、模型不存在、
			// 凭据无权、体过大…）照常 failover，但不能把整条渠道打死。
			// 流式已提交响应后上游半路断流属于传输层故障，按故障计。
			if channelFailure(callErr, status) {
				breakerFail(cand.provider)
			}
			kind := errKindTransport
			switch {
			case status >= 400:
				kind = classifyUpstreamStatus(status)
			case status >= 200 && status < 300:
				// 流式响应已开始后上游中断（客户端看到截断）
				kind = errKindStreamAborted
			}
			h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, kind, status, upstreamFailureDetail(callErr), start)
			lastStatus = status
			lastErrMsg = "upstream " + cand.provider.Name + " failed: " + callErr.Error()
			lastErrDetail = ""
			var ue *UpstreamError
			if errors.As(callErr, &ue) {
				lastErrDetail = ue.Detail()
			} else if d, ok := callErr.(interface{ Detail() string }); ok {
				// 其他自带响应摘要的错误（如 codexNonSSEError）：摘要只回客户端
				lastErrDetail = d.Detail()
			}
			// 只有已经向客户端提交了成功响应的流才不能 failover。
			// status > 0 仅表示上游返回了 HTTP 状态，4xx/5xx 分支尚未
			// 写客户端响应，不能把它误判成“流已开始”，否则会返回空 200。
			if req.Stream && status >= http.StatusOK && status < http.StatusMultipleChoices {
				return
			}
			continue
		}
		if callErr == errClientGone {
			// Codex 在工具调用轮次切换、取消旧请求或退出会话时会主动断开
			// SSE。客户端断开不代表上游渠道故障，不能累计熔断失败；否则一次
			// 工具循环中的快速取消就可能把健康渠道熔断，后续请求全部 503。
			return
		}

		if shouldFailover(status, nil) {
			// 上游返回了 429/5xx 但没附带 error（非标准实现）：与 error 路径共用分类
			breakerFail(cand.provider)
			lastStatus = status
			lastErrMsg = "upstream " + cand.provider.Name + " returned " + http.StatusText(status)
			h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, classifyUpstreamStatus(status), status,
				lastErrMsg+"（上游未返回错误详情）", start)
			if req.Stream && status > 0 {
				return // 流已透传错误状态，无法重试
			}
			continue
		}

		// 成功
		if breakerCheckEnabled(cand.provider) {
			h.Breaker.OnSuccess(cand.provider.ID)
		}
		return
	}

	if lastStatus == 0 {
		lastStatus = http.StatusServiceUnavailable
	}
	log.Printf("[relay] all channels failed for model %s user=%d: %s", req.Model, userID, lastErrMsg)
	clientMsg := lastErrMsg
	if lastErrDetail != "" {
		clientMsg += ": " + lastErrDetail
	}
	gatewayError(w, entry, lastStatus, "all channels failed: "+clientMsg)
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// recordUsage 异步记账；UsageWriter 未注入时退化为直接落库。
func (h *Handler) recordUsage(ctx context.Context, userID, apiKeyID, providerID int64, provider *model.Provider, modelName string, usage UsageRecord, status int, latency time.Duration) {
	// 计费：按入口模型名查价（价格快照随记录落库）；未命中且渠道配了
	// BillingMap 时按归一化标准名计费。失败请求 tokens=0 → cost=0。
	var cost float64
	var billingName string
	if h.Pricing != nil {
		cost = h.Pricing.CostOf(modelName, provider, usage.PromptTokens, usage.CompletionTokens)
		// 归一化后的计费名（聚合按它归并；明细保留真实请求名）
		billingName = h.Pricing.BillingNameOf(modelName, provider)
	}
	// 额度实时累加：拿到实际费用立刻进 Redis 计数（DB 权威值随 flush 批量对账）
	if cost > 0 {
		h.Quota.Add(ctx, apiKeyID, cost)
	}
	e := usageEntry{
		userID:           userID,
		apiKeyID:         apiKeyID,
		providerID:       providerID,
		model:            modelName,
		billingModel:     billingName,
		promptTokens:     usage.PromptTokens,
		completionTokens: usage.CompletionTokens,
		statusCode:       status,
		latencyMs:        int(latency.Milliseconds()),
		cost:             cost,
	}
	if h.Usage != nil {
		h.Usage.Record(e)
		return
	}
	ul := &model.UsageLog{
		UserID:           e.userID,
		ApiKeyID:         e.apiKeyID,
		ProviderID:       e.providerID,
		Model:            e.model,
		BillingModel:     e.billingModel,
		PromptTokens:     e.promptTokens,
		CompletionTokens: e.completionTokens,
		StatusCode:       e.statusCode,
		LatencyMs:        e.latencyMs,
		Cost:             e.cost,
	}
	_ = h.Op.FlushUsageBatch([]*model.UsageLog{ul}, map[int64]float64{ul.ApiKeyID: ul.Cost})
}

// noteFailure 记录一次失败诊断（异步；ErrorWriter 未注入时直接落库）。
//
// 调用点覆盖「无候选渠道 / 熔断中 / 凭据问题 / 协议转换 / 代理配置 / 上游调用」
// 全部失败分支；客户端主动断开不计（不是渠道问题）。
func (h *Handler) noteFailure(userID, apiKeyID, providerID int64, modelName, kind string, status int, msg string, start time.Time) {
	if h.Op == nil || kind == "" {
		return
	}
	row := &model.RequestError{
		UserID:     userID,
		ProviderID: providerID,
		ApiKeyID:   apiKeyID,
		Model:      modelName,
		Kind:       kind,
		StatusCode: status,
		Message:    truncateErrMessage(normalizeErrWhitespace(msg)),
		LatencyMs:  int(time.Since(start).Milliseconds()),
	}
	if h.ErrSink != nil {
		h.ErrSink.Record(row)
		return
	}
	_ = h.Op.InsertRequestErrors([]*model.RequestError{row})
}

// upstreamFailureDetail 生成可落库的失败摘要。
//
// 上游非 2xx 正文可能回显 prompt 片段（见 UpstreamError.Detail 注释），所以只取
// 结构化错误字段（reqerr.sanitizeErrMessage）；其余错误类型直接用 Error()——
// 它本身就是协议层描述（状态码/content-type），不含上游正文。
func upstreamFailureDetail(err error) string {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if s := sanitizeErrMessage(ue.Body, ue.ContentType); s != "" {
			return s
		}
		return fmt.Sprintf("upstream %d（无错误详情）", ue.Status)
	}
	return err.Error()
}

// buildUpstreamBody 把请求体里的模型名替换为上游名，并把 developer role 归一化
// 为 system（其余字段原样保留）。
//
// developer 来自 Responses API / OpenAI 新模型；Codex 客户端经 /v1/responses →
// pivot 转换后常携带 developer 消息，而绝大多数 OpenAI 兼容上游（DeepSeek、GLM、
// Kimi 等）的 role 枚举只有 system/user/assistant/tool，收到 developer 会 400
// 拒绝整条请求。
func buildUpstreamBody(original []byte, reqModel, upstreamModel string) []byte {
	needModel := reqModel != upstreamModel
	needRole := bytes.Contains(original, developerRoleToken)
	if !needModel && !needRole {
		return original
	}
	var m map[string]any
	if err := json.Unmarshal(original, &m); err != nil {
		return original
	}
	if needRole {
		normalizeDeveloperRoles(m)
	}
	if needModel {
		m["model"] = upstreamModel
	}
	out, err := json.Marshal(m)
	if err != nil {
		return original
	}
	return out
}

// developerRoleToken developer role 的字节探测串：命中才做 JSON 重写，
// 无 developer 的热路径零解析开销。
var developerRoleToken = []byte(`"developer"`)

// normalizeDeveloperRoles 把 messages[].role == "developer" 归一化为 "system"。
func normalizeDeveloperRoles(body map[string]any) {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := m["role"].(string); r == "developer" {
			m["role"] = "system"
		}
	}
}

// gatewayError 按入口协议返回错误体（openai/responses：{error:{...}}；
// anthropic：{type:error,error:{type,message}}）。
func gatewayError(w http.ResponseWriter, entry string, status int, msg string) {
	if entry == protoAnthropic {
		resp.JSON(w, status, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": msg,
			},
		})
		return
	}
	resp.JSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "gateway_error"},
	})
}

// maintSuffix 维护公告拼接（空公告不加尾巴）。
func maintSuffix(text string) string {
	if text == "" {
		return ""
	}
	return ": " + text
}

// ---- 上游调用：openai chat/completions 渠道 ----

// upstreamCall 执行一次 openai 协议上游调用。
// entry=chat：非流式字节透传、流式 SSE 透传（原始行为）；
// entry=messages/responses：响应需回转成入口格式（经 relayAgg / 流式桥）。
// client 由调用方按渠道代理开关选定（clientFor）。
// 返回 (HTTP 状态码, usage 记录, 错误)。
func (h *Handler) upstreamCall(
	ctx context.Context,
	client *http.Client,
	target string,
	apiKey string,
	body []byte,
	isStream bool,
	entry string,
	w http.ResponseWriter,
	ua string,
) (int, UsageRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return 0, UsageRecord{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if isStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
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

	if isStream {
		// 流式：chat 入口透传；messages/responses 入口走桥转换
		if entry == protoOpenAI {
			return streamPassthrough(w, upResp)
		}
		return bridgeChatChunksToEntry(w, upResp, entry, upModelFromBody(body))
	}

	respBytes, err := io.ReadAll(upResp.Body)
	if err != nil {
		return 0, UsageRecord{}, err
	}

	if entry != protoOpenAI {
		// 非流式：解析 openai 响应 → relayAgg → 入口格式 JSON
		agg, ok := openaiJSONToAgg(respBytes)
		if !ok {
			// 非标准响应体：原样透传（客户端自行处理）
			writeRawJSON(w, upResp.StatusCode, respBytes)
			return upResp.StatusCode, UsageRecord{}, nil
		}
		if agg.ErrMsg != "" && agg.Content == "" && len(agg.ToolCalls) == 0 {
			return upResp.StatusCode, UsageRecord{}, fmt.Errorf("openai: %s", agg.ErrMsg)
		}
		usage := UsageRecord{PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens, Found: agg.PromptTokens > 0 || agg.CompletionTokens > 0}
		writeEntryJSON(w, entry, upModelFromBody(body), agg)
		return upResp.StatusCode, usage, nil
	}

	u := extractUsageFromJSON(respBytes)
	ct := upResp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(upResp.StatusCode)
	_, _ = w.Write(respBytes)
	return upResp.StatusCode, u, nil
}

// openaiJSONToAgg openai chat/completions 非流式响应 → relayAgg。
func openaiJSONToAgg(data []byte) (relayAgg, bool) {
	var r struct {
		Choices []struct {
			Message struct {
				Content          any    `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
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
	agg := relayAgg{FinishReason: "stop"}
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		agg.Content = joinMessageText(c.Message.Content)
		agg.Reasoning = c.Message.ReasoningContent
		for _, tc := range c.Message.ToolCalls {
			agg.HasToolCall = true
			agg.ToolCalls = append(agg.ToolCalls, aggToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		if c.FinishReason != nil {
			agg.FinishReason = finishFromOpenAIFinish(*c.FinishReason)
		}
	}
	if r.Usage != nil {
		agg.PromptTokens = r.Usage.PromptTokens
		agg.CompletionTokens = r.Usage.CompletionTokens
	}
	return agg, true
}

// writeEntryJSON relayAgg → 入口协议非流式响应。
func writeEntryJSON(w http.ResponseWriter, entry, model string, agg relayAgg) {
	switch entry {
	case protoAnthropic:
		writeAnthropicMessageJSON(w, model, agg)
	case protoResponses:
		writeResponsesJSON(w, model, agg)
	default:
		writeChatCompletionJSON(w, model, agg)
	}
}

// writeRawJSON 原样透传 JSON 响应体。
func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
