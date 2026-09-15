package handlers

// providermodels_stream.go 流式模拟真实请求测试（打字机增强）：
// - TestModelStream POST /api/providers/{id}/test_model_stream —— 与 test_model 同参数
//   （固定流式），但不聚合完整响应：逐块解析上游 SSE，把内容/思考增量以 SSE 事件
//   实时推给前端，实现打字机输出；结束时推 done 事件携带完整统计（耗时/首字延迟/
//   finish_reason/token 用量）。
// 请求构建失败（渠道不存在/参数非法/凭据问题）仍走普通 JSON 响应，前端按错误处理；
// 一旦切换为 SSE（HTTP 200 + text/event-stream），后续一切结果以事件表达。
// 只读探测，与 TestModel 相同约定：不写库、不触发 oauth 兜底刷新。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// streamSSEEvent 推给前端的事件帧。type：
//   - delta：内容增量（content_delta / reasoning_delta）
//   - done ：流结束，携带完整统计；err 非空表示流中途异常
//   - error：上游 4xx/5xx（响应头阶段即失败），无内容可推
type streamSSEEvent struct {
	Type             string `json:"type"`
	ContentDelta     string `json:"content_delta,omitempty"`
	ReasoningDelta   string `json:"reasoning_delta,omitempty"`
	OK               bool   `json:"ok,omitempty"`
	Status           int    `json:"status,omitempty"`
	FirstTokenMs     int64  `json:"first_token_ms,omitempty"`
	LatencyMs        int64  `json:"latency_ms,omitempty"`
	Model            string `json:"model,omitempty"`
	Stream           bool   `json:"stream,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	Error            string `json:"error,omitempty"`
	Channel          string `json:"channel_name,omitempty"`
}

// writeSSE 写一条 SSE data 帧并 flush；写出失败（客户端断开）返回 false。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev streamSSEEvent) bool {
	buf, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// TestModelStream POST /api/providers/{id}/test_model_stream —— 流式真实请求测试。
// 参数同 testModelReq（stream 固定 true），响应为 SSE 事件流。
func (h *TestHandler) TestModelStream(w http.ResponseWriter, r *http.Request) {
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

	res := testModelResult{Channel: p.Name, Stream: true}
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

	upReq, err := buildUpstreamChatProbeReq(r.Context(), p, secret, accountID, res.Model, prompt, maxTokens, true)
	if err != nil {
		res.Error = err.Error()
		emit()
		return
	}

	start := time.Now()
	respUp, err := client.Do(upReq)
	if err != nil {
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Error = err.Error()
		emit()
		return
	}
	defer respUp.Body.Close()

	if respUp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(respUp.Body, 4<<10))
		res.Status = respUp.StatusCode
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Error = "model unavailable (HTTP " + itoa(respUp.StatusCode) + "): " + truncateBody(string(buf), 256)
		emit()
		return
	}

	// 上游 2xx：切换为 SSE，此后所有结果以事件推进
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, okFlush := w.(http.Flusher)
	if !okFlush {
		res.Error = "streaming unsupported by server"
		emit()
		return
	}

	pushStreamChat(w, flusher, r.Context(), p, respUp.Body, start, res.Model)
}

// pushStreamChat 读上游流式响应并向前端逐块推送 SSE 事件，直至流结束。
// api_key 渠道：逐行解析 chat.completions chunks（与 readStreamChat 同构）；
// Codex 渠道：responses SSE 事件增量回调（AggregateCodexStreamOnDelta）。
func pushStreamChat(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, p *model.Provider, body io.Reader, start time.Time, modelName string) {
	pr := chatProbe{Status: http.StatusOK}

	sendDelta := func(contentDelta, reasoningDelta string) bool {
		if contentDelta == "" && reasoningDelta == "" {
			return true
		}
		if pr.FirstTokenMs == 0 {
			pr.FirstTokenMs = time.Since(start).Milliseconds()
		}
		if ctx.Err() != nil {
			return false
		}
		return writeSSE(w, flusher, streamSSEEvent{Type: "delta", ContentDelta: contentDelta, ReasoningDelta: reasoningDelta})
	}

	finish := func(errMsg string) {
		pr.LatencyMs = time.Since(start).Milliseconds()
		if pr.FirstTokenMs == 0 {
			pr.FirstTokenMs = pr.LatencyMs
		}
		ev := streamSSEEvent{
			Type:             "done",
			OK:               errMsg == "",
			Status:           pr.Status,
			FirstTokenMs:     pr.FirstTokenMs,
			LatencyMs:        pr.LatencyMs,
			Model:            modelName,
			Stream:           true,
			FinishReason:     pr.FinishReason,
			PromptTokens:     pr.PromptTokens,
			CompletionTokens: pr.CompletionTokens,
			Error:            errMsg,
			Channel:          p.Name,
		}
		if ev.FinishReason == "" && errMsg == "" {
			ev.FinishReason = "stop"
		}
		writeSSE(w, flusher, ev)
	}

	if relay.IsCodexProvider(p) {
		agg, aggErr := relay.AggregateCodexStreamOnDelta(body, func(contentDelta, reasoningDelta string) {
			sendDelta(contentDelta, reasoningDelta)
		})
		pr.Content, pr.Reasoning = agg.Content, agg.Reasoning
		pr.FinishReason = agg.FinishReason
		pr.PromptTokens, pr.CompletionTokens = agg.PromptTokens, agg.CompletionTokens
		errMsg := agg.ErrMsg
		if aggErr != nil && agg.Content == "" {
			errMsg = aggErr.Error()
		}
		finish(errMsg)
		return
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
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
			d := chunk.Choices[0].Delta
			if !sendDelta(d.Content, pickReasoning(d.ReasoningContent, d.Reasoning, d.ReasoningText)) {
				return // 客户端已断开
			}
			if chunk.Choices[0].FinishReason != nil {
				pr.FinishReason = *chunk.Choices[0].FinishReason
			}
		}
		if chunk.Usage != nil {
			pr.PromptTokens = chunk.Usage.PromptTokens
			pr.CompletionTokens = chunk.Usage.CompletionTokens
		}
	}
	errMsg := ""
	if err := sc.Err(); err != nil {
		errMsg = "stream interrupted: " + err.Error()
	}
	finish(errMsg)
}
