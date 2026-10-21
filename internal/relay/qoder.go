package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/qoder"
	"github.com/chengongliang/keygrid/internal/workbuddy"
)

// qoder.go Qoder 渠道转发（COSY 签名 + 私有 /algo 协议）。
//
// 为什么不能走 OpenAI 兼容路径：api3.qoder.sh 上没有 /v1/chat/completions 与
// /v1/models（实测 404），对话只有
//
//	POST {chatBase}/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1
//
// 一个端点，且正文要 WAF 混淆编码 + 17 个 Cosy-* 签名头，响应是
// {statusCodeValue, body} 信封包着的 OpenAI chunk。本文件负责把 relay 的 pivot
// 请求送进去、再把信封拆回 OpenAI SSE（internal/qoder 提供协议细节）。
//
// 上游固定 stream:true：非流式客户端由我们折叠成一条 chat.completion JSON。

// IsQoderProvider 判断渠道是否走 Qoder 私有协议。
func IsQoderProvider(p *model.Provider) bool { return qoder.IsQoder(p) }

// qoderRuntime 取 Qoder 缓存实例（nil 时用进程级默认，渠道面板与转发共享）。
func (h *Handler) qoderRuntime() *qoder.Runtime {
	if h.Qoder != nil {
		return h.Qoder
	}
	return qoder.Default()
}

// upstreamCallQoder 向 Qoder 私有端点发起一次对话请求。
//
// target 参数不参与 URL 拼接（端点由区域与 token 类型决定）；保留签名只为与
// upstreamCall 系列保持一致，便于调用点复用。
func (h *Handler) upstreamCallQoder(
	ctx context.Context,
	client *http.Client,
	p *model.Provider,
	token string,
	tokenExtra map[string]string,
	upModel string,
	openaiBody []byte,
	isStream bool,
	entry string,
	w http.ResponseWriter,
	ua string,
) (int, UsageRecord, error) {
	rt := h.qoderRuntime()
	region := qoder.RegionOf(p)

	rc, err := rt.ResolveCredential(ctx, client, region, token, tokenExtra)
	if err != nil {
		// 凭据层面问题（PAT 兑换失败 / 缺 token）：401 语义，提示重新授权。
		if ue, ok := err.(*qoder.UpstreamError); ok {
			return ue.Status, UsageRecord{}, &UpstreamError{Status: ue.Status, Body: ue.Body}
		}
		return http.StatusUnauthorized, UsageRecord{}, fmt.Errorf("qoder credential: %w", err)
	}
	if rc.UserID == "" {
		return http.StatusUnauthorized, UsageRecord{}, errors.New(
			"qoder credential is missing userId (COSY signing requires it); reconnect the account")
	}

	// 模型目录：chat 必须回传该模型的 model_config，缺了无法构造请求。
	cat, err := rt.Catalog(ctx, client, rc, false)
	if err != nil {
		return upstreamStatusOf(err), UsageRecord{}, fmt.Errorf("qoder model catalog: %w", err)
	}

	built, err := qoder.BuildRequest(ctx, qoder.BuildInput{
		OpenAIBody: openaiBody,
		ModelKey:   upModel,
		UserID:     rc.UserID,
		Catalog:    cat,
		Uploader:   qoder.HTTPUploader{Client: client, RC: rc},
	}, rc)
	if err != nil {
		return http.StatusBadRequest, UsageRecord{}, err
	}

	target := qoder.ChatURL(rc.Region, rc.Token)
	body := qoder.EncodeBody(built.Payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return 0, UsageRecord{}, err
	}
	if err := qoder.ApplyHeaders(req, body, target, rc.Creds()); err != nil {
		return http.StatusUnauthorized, UsageRecord{}, fmt.Errorf("qoder cosy signing: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	// gzip 会触发上游 CDN 的签名校验，必须强制 identity。
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Model-Key", built.ModelKey)
	req.Header.Set("X-Model-Source", built.ModelSource)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	} else {
		req.Header.Set("User-Agent", "qodercli/1.0.0")
	}
	ApplyUpstreamHeaders(req.Header, p)

	resp, err := client.Do(req)
	if err != nil {
		return 0, UsageRecord{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := &UpstreamError{
			Status:      resp.StatusCode,
			Body:        string(raw),
			ContentType: resp.Header.Get("Content-Type"),
			RetryAfter:  parseRetryAfter(resp.Header.Get("Retry-After")),
		}
		if qoder.IsBillingError(err) {
			err.Status = http.StatusTooManyRequests
		}
		return err.Status, UsageRecord{}, err
	}

	stream := qoder.NewStream(resp.Body, resp.Body, built.ModelKey)
	// 首帧信封错误（余额不足 / 参数错 / 登录过期）要在写 200 之前翻成真状态码，
	// 否则客户端会把错误当成功响应收下。
	if perr := stream.Peek(); perr != nil {
		var ue *qoder.UpstreamError
		if errors.As(perr, &ue) {
			// 计费/配额拦截按 429 报：让 failover 继续尝试其它渠道，
			// 而不是当「请求参数错」直接判死。
			if qoder.IsBillingError(ue) {
				return http.StatusTooManyRequests, UsageRecord{},
					&UpstreamError{Status: http.StatusTooManyRequests, Body: ue.Body}
			}
			return ue.Status, UsageRecord{}, &UpstreamError{Status: ue.Status, Body: ue.Body}
		}
		return http.StatusBadGateway, UsageRecord{}, perr
	}

	upResp := &http.Response{StatusCode: http.StatusOK, Body: stream, Header: http.Header{}}

	if isStream {
		if entry == protoOpenAI {
			// OpenAI 入口：拆封后的 chunk 直接透传（streamPassthrough 会顺带提取 usage）。
			return streamPassthrough(w, upResp)
		}
		return bridgeChatChunksToEntry(w, upResp, entry, upModel)
	}

	// 客户端要非流式：上游只有 SSE，读完折叠成一条 chat.completion。
	raw, err := io.ReadAll(io.LimitReader(stream, 32<<20))
	if err != nil {
		return http.StatusBadGateway, UsageRecord{}, err
	}
	merged, ok := workbuddy.CollapseSSE(raw)
	if !ok {
		return http.StatusBadGateway, UsageRecord{}, &UpstreamError{
			Status: http.StatusBadGateway,
			Body:   "qoder: upstream stream produced no usable content",
		}
	}

	if entry != protoOpenAI {
		agg, parsed := openaiJSONToAgg(merged)
		if !parsed {
			writeRawJSON(w, http.StatusOK, merged)
			return http.StatusOK, UsageRecord{}, nil
		}
		if agg.ErrMsg != "" && agg.Content == "" && len(agg.ToolCalls) == 0 {
			return http.StatusBadGateway, UsageRecord{}, &UpstreamError{Status: http.StatusBadGateway, Body: agg.ErrMsg}
		}
		usage := UsageRecord{
			PromptTokens:     agg.PromptTokens,
			CompletionTokens: agg.CompletionTokens,
			Found:            agg.PromptTokens > 0 || agg.CompletionTokens > 0,
		}
		writeEntryJSON(w, entry, upModel, agg)
		return http.StatusOK, usage, nil
	}

	u := extractUsageFromJSON(merged)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(merged)
	return http.StatusOK, u, nil
}

// upstreamStatusOf 从错误里提取上游 HTTP 状态（未知按 502）。
func upstreamStatusOf(err error) int {
	var ue *qoder.UpstreamError
	if errors.As(err, &ue) && ue.Status >= 400 && ue.Status <= 599 {
		return ue.Status
	}
	var re *UpstreamError
	if errors.As(err, &re) && re.Status >= 400 && re.Status <= 599 {
		return re.Status
	}
	return http.StatusBadGateway
}
