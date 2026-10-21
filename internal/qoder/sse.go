package qoder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sse.go Qoder SSE 信封拆封，移植自 9router
// open-sse/shared/qoder/sse.js + executors/qoder.js（wrapQoderSSE）。
//
// 上游每一行是 {statusCodeValue, body} 信封，body 里才是 OpenAI 风格 chunk
// （或 "[DONE]"）。另外两个上游怪癖：
//   - finish_reason 常常挂在 delta 上，而不是 choice 上；
//   - token 用量在 finish 之后的 `choices: []` 帧里 —— 下游客户端只看 finish
//     帧的 usage，所以要把两帧合并成一条“终端 chunk”。
//
// 上游在发出终止帧后往往仍保持 socket 不关（agent keepalive），所以见到终止帧
// 必须立刻放弃上游连接，否则非流式客户端会一直挂着。

// maxSSELine 单行上限（正文是 base64 之外的小 JSON，4MB 足够防御异常流）。
const maxSSELine = 4 << 20

// Stream 把上游信封流解成 OpenAI SSE 字节流（实现 io.Reader）。
type Stream struct {
	br         *bufio.Reader
	closer     io.Closer
	model      string
	out        bytes.Buffer
	terminal   bool
	firstFrame bool

	// coalescer 状态
	pendingFinish   string
	pendingUsage    json.RawMessage
	lastID          string
	lastModel       string
	lastCreated     int64
	finishForwarded bool
	doneEmitted     bool

	// err 上游错误（peek 阶段捕获）
	err error
}

// NewStream 包装上游响应体。closer 在终止时立即关闭（可为 nil）。
func NewStream(r io.Reader, closer io.Closer, model string) *Stream {
	return &Stream{br: bufio.NewReaderSize(r, 32*1024), closer: closer, model: model, firstFrame: true}
}

// Peek 预读首帧：若上游以错误信封开场（典型：余额不足 / 参数错误 / 登录过期），
// 返回 *UpstreamError，调用方据此返回真实 HTTP 状态而不是假的 200 流。
// 成功时把首帧的 OpenAI 内容留在输出缓冲里，绝不丢数据。
func (s *Stream) Peek() error {
	for {
		if s.err != nil {
			return s.err
		}
		if s.out.Len() > 0 || s.terminal {
			return nil
		}
		if err := s.step(); err != nil {
			if err == io.EOF {
				s.terminal = true
				return nil
			}
			return err
		}
	}
}

// Read 实现 io.Reader。
func (s *Stream) Read(p []byte) (int, error) {
	for s.out.Len() == 0 {
		if s.terminal {
			return 0, io.EOF
		}
		if err := s.step(); err != nil {
			if err == io.EOF {
				s.terminal = true
				s.closeUpstream()
				break
			}
			if s.out.Len() > 0 {
				break // 已产出的内容优先交给调用方，错误留到下一轮
			}
			return 0, err
		}
	}
	return s.out.Read(p)
}

// Close 实现 io.ReadCloser：终止时立刻放弃上游连接。
func (s *Stream) Close() error {
	s.terminal = true
	s.closeUpstream()
	return nil
}

// closeUpstream 立即放弃上游连接（终止帧后上游可能仍挂着 keepalive）。
func (s *Stream) closeUpstream() {
	if s.closer != nil {
		_ = s.closer.Close()
		s.closer = nil
	}
}

// readLine 读一行（带长度上限）。
func (s *Stream) readLine() (string, error) {
	var buf []byte
	for {
		frag, err := s.br.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > maxSSELine {
			return "", fmt.Errorf("qoder sse: line exceeds %d bytes", maxSSELine)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(buf), err
	}
}

// step 处理上游一行。
func (s *Stream) step() error {
	line, err := s.readLine()
	trimmed := strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(strings.TrimSpace(trimmed), "data:") {
		s.handleDataLine(strings.TrimSpace(trimmed)[len("data:"):])
	} else if strings.TrimSpace(trimmed) != "" {
		// 兼容：个别上游把信封直接当裸 JSON 行发出
		s.handleDataLine(trimmed)
	}
	if s.doneEmitted {
		s.terminal = true
		s.closeUpstream()
	}
	if err != nil {
		if err == io.EOF {
			// 流自然结束：补终端帧
			s.flush()
			return io.EOF
		}
		return err
	}
	return nil
}

// handleDataLine 处理一条 data 负载。
func (s *Stream) handleDataLine(data string) {
	data = strings.TrimSpace(data)
	if data == "" {
		return
	}
	if data == "[DONE]" {
		s.flush()
		return
	}

	var envelope struct {
		StatusCodeValue any             `json:"statusCodeValue"`
		Body            json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		// 不是 JSON：原样透传（保真优先，不因解析失败静默丢内容）
		s.firstFrame = false
		s.emitRawLine(data)
		return
	}
	// 裸 OpenAI chunk（没有 statusCodeValue / body 两个信封字段）：直接透传。
	if envelope.StatusCodeValue == nil && len(envelope.Body) == 0 {
		s.firstFrame = false
		s.handleInner(data)
		return
	}
	status := 200
	if envelope.StatusCodeValue != nil {
		status = intFromAny(envelope.StatusCodeValue, 200)
	}
	inner := innerBody(envelope.Body)

	if status != 200 {
		// 首帧错误：交给 Peek 转成 HTTP 错误；流中错误：发结构化错误块并终止。
		if s.firstFrame {
			s.err = &UpstreamError{Status: mapUpstreamStatus(status, inner), Body: truncate(inner, 512)}
			return
		}
		if isBillingBlock(inner) {
			s.emitJSON(map[string]any{
				"error": map[string]any{
					"message": truncate(inner, 512),
					"code":    "qoder_billing_block",
					"status":  403,
					"type":    "quota_error",
				},
			})
		} else {
			s.emitJSON(map[string]any{
				"id":      fmt.Sprintf("qoder-error-%d", nowUnixMillis()),
				"object":  "chat.completion.chunk",
				"created": nowUnix(),
				"model":   s.model,
				"choices": []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{"content": "\n[qoder error " + fmt.Sprint(status) + ": " + truncate(inner, 200) + "]"},
					"finish_reason": "stop",
				}},
			})
		}
		s.emitDone()
		return
	}
	s.firstFrame = false
	if inner == "" {
		return
	}
	s.handleInner(inner)
}

// handleInner 处理信封内的 OpenAI chunk。
func (s *Stream) handleInner(inner string) {
	if inner == "[DONE]" {
		s.flush()
		return
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(inner), &parsed); err != nil {
		// 非 JSON：原样转发（保真优先）
		s.emitRawLine(inner)
		return
	}

	if id, ok := parsed["id"].(string); ok && id != "" {
		s.lastID = id
	}
	if m, ok := parsed["model"].(string); ok && m != "" {
		s.lastModel = m
	}
	if c, ok := parsed["created"].(float64); ok {
		s.lastCreated = int64(c)
	}
	if usage, ok := parsed["usage"]; ok && usage != nil {
		if raw, err := jsonMarshal(canonicalizeUsage(usage)); err == nil && string(raw) != "null" {
			s.pendingUsage = raw
		}
	}

	finish := finishReasonOf(parsed)
	if hasValuableDelta(parsed) {
		s.emitRawLine(inner)
		if finish != "" {
			s.finishForwarded = true
			if len(s.pendingUsage) > 0 {
				s.pendingFinish = finish
			}
		}
		if s.pendingFinish != "" && len(s.pendingUsage) > 0 {
			s.emitTerminal()
			s.emitDone()
		}
		return
	}

	if finish != "" {
		s.pendingFinish = finish
	}
	if (s.pendingFinish != "" || s.finishForwarded) && len(s.pendingUsage) > 0 {
		if s.pendingFinish == "" {
			s.pendingFinish = "stop"
		}
		s.emitTerminal()
		s.emitDone()
	}
}

// flush 把缓存中的 finish/usage 合成为终端 chunk，然后发 [DONE]。
func (s *Stream) flush() {
	if s.doneEmitted {
		return
	}
	if len(s.pendingUsage) > 0 || (s.pendingFinish != "" && !s.finishForwarded) {
		s.emitTerminal()
	}
	s.emitDone()
}

// emitTerminal 输出合并后的终端 chunk（finish_reason + usage）。
func (s *Stream) emitTerminal() {
	if s.pendingFinish == "" && len(s.pendingUsage) == 0 {
		return
	}
	finish := s.pendingFinish
	if finish == "" {
		finish = "stop"
	}
	chunk := map[string]any{
		"id":      s.metaID(),
		"object":  "chat.completion.chunk",
		"created": s.metaCreated(),
		"model":   s.metaModel(),
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	if len(s.pendingUsage) > 0 {
		var usage any
		if err := json.Unmarshal(s.pendingUsage, &usage); err == nil {
			chunk["usage"] = usage
		}
	}
	s.emitJSON(chunk)
	s.pendingFinish = ""
	s.pendingUsage = nil
	s.finishForwarded = false
}

// emitJSON 输出一条 data 行（单行，换行截断）。
func (s *Stream) emitJSON(v any) {
	raw, err := jsonMarshal(v)
	if err != nil {
		return
	}
	s.emitRawLine(string(raw))
}

// emitRawLine 输出一条 data 行。
func (s *Stream) emitRawLine(payload string) {
	payload = strings.ReplaceAll(payload, "\r", "")
	payload = strings.ReplaceAll(payload, "\n", "")
	s.out.WriteString("data: ")
	s.out.WriteString(payload)
	s.out.WriteString("\n\n")
}

// emitDone 输出 [DONE]。
func (s *Stream) emitDone() {
	if s.doneEmitted {
		return
	}
	s.out.WriteString("data: [DONE]\n\n")
	s.doneEmitted = true
}

func (s *Stream) metaID() string {
	if s.lastID != "" {
		return s.lastID
	}
	return fmt.Sprintf("qoder-%d", nowUnixMillis())
}

func (s *Stream) metaCreated() int64 {
	if s.lastCreated > 0 {
		return s.lastCreated
	}
	return nowUnix()
}

func (s *Stream) metaModel() string {
	if s.lastModel != "" {
		return s.lastModel
	}
	return s.model
}

// ---- 信封工具 ----

// innerBody 取信封 body（字符串直接返回，对象序列化）。
func innerBody(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// intFromAny JSON 值 → int。
func intFromAny(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		var n int
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

// mapUpstreamStatus 上游业务码 → HTTP 状态（首帧错误返回给客户端用）。
func mapUpstreamStatus(status int, inner string) int {
	if isBillingBlock(inner) {
		return 403
	}
	if status >= 400 && status <= 599 {
		return status
	}
	return 502
}

// IsBillingError 判断错误是否为计费/配额拦截。
// relay 据此把 4xx 改判成 429：这类错误应 failover 到其它渠道，而不是当成
// 「请求参数错误」直接回给客户端。
func IsBillingError(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return isBillingBlock(ue.Body) || isBillingBlock(ue.Error())
	}
	return false
}

// isBillingBlock 判断是否计费/配额拦截。
// 特征：code 110（计费次数超限）、112（额度耗尽）、10605（队列限流）、pricingUrl。
func isBillingBlock(inner string) bool {
	if inner == "" {
		return false
	}
	lower := strings.ToLower(inner)
	if strings.Contains(lower, "pricingurl") {
		return true
	}
	var parsed struct {
		Code any `json:"code"`
	}
	if err := json.Unmarshal([]byte(inner), &parsed); err == nil {
		code := strings.TrimSpace(fmt.Sprint(parsed.Code))
		switch code {
		case "110", "112", "10605":
			return true
		}
	}
	return strings.Contains(inner, `"code":"112"`) || strings.Contains(inner, `"code":"10605"`)
}

// finishReasonOf finish_reason 可能挂在 choice 或 delta 上。
func finishReasonOf(parsed map[string]any) string {
	choices, _ := parsed["choices"].([]any)
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				return fr
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if fr, ok := delta["finish_reason"].(string); ok && fr != "" {
					return fr
				}
			}
		}
	}
	if fr, ok := parsed["finish_reason"].(string); ok {
		return fr
	}
	return ""
}

// hasValuableDelta 该 chunk 是否含实际内容（内容 / 推理 / 工具调用 / role）。
func hasValuableDelta(parsed map[string]any) bool {
	choices, _ := parsed["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return false
	}
	if s, ok := delta["content"].(string); ok && s != "" {
		return true
	}
	if s, ok := delta["reasoning_content"].(string); ok && s != "" {
		return true
	}
	if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	if role, ok := delta["role"].(string); ok && role != "" {
		return true
	}
	return false
}

// canonicalizeUsage 归一 usage（兼容 input_tokens/output_tokens 与缓存命中字段）。
func canonicalizeUsage(usage any) any {
	m, ok := usage.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	num := func(v any) (int, bool) {
		switch t := v.(type) {
		case float64:
			return int(t), true
		}
		return 0, false
	}
	prompt, hasPrompt := num(m["prompt_tokens"])
	if !hasPrompt {
		prompt, hasPrompt = num(m["input_tokens"])
	}
	completion, hasCompletion := num(m["completion_tokens"])
	if !hasCompletion {
		completion, hasCompletion = num(m["output_tokens"])
	}
	if !hasPrompt && !hasCompletion {
		return nil
	}

	details := map[string]any{}
	if d, ok := m["prompt_tokens_details"].(map[string]any); ok {
		for k, v := range d {
			details[k] = v
		}
	}
	cached, cachedOK := num(details["cached_tokens"])
	if !cachedOK {
		cached, cachedOK = num(m["cached_tokens"])
	}
	if !cachedOK {
		cached, cachedOK = num(m["prompt_cache_hit_tokens"])
	}
	if !cachedOK {
		cached, cachedOK = num(m["cache_read_input_tokens"])
	}
	if cachedOK {
		details["cached_tokens"] = cached
	}

	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	if v, ok := num(m["total_tokens"]); ok {
		out["total_tokens"] = v
	}
	if cachedOK {
		out["cached_tokens"] = cached
	}
	if len(details) > 0 {
		out["prompt_tokens_details"] = details
	}
	if d, ok := m["completion_tokens_details"].(map[string]any); ok {
		out["completion_tokens_details"] = d
	}
	return out
}
