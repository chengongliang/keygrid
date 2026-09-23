package relay

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// stream_bridge.go 流式协议桥：任意上游流 → 入口协议 SSE。
//
// 分层策略（避免 3×3 全组合状态机）：
//
//	上游 openai   ──(chat chunks,原生)──────────────┐
//	上游 anthropic ─(anthropicInState → chat chunks)─┼─→ entry 发射器
//	上游 responses ─(convertCodexEvent → chat chunks)┘
//
// entry 发射器：
//	chat      → chat chunks 原样透传（含 [DONE]）
//	messages  → anthropicOutState：chat chunks → anthropic SSE 事件
//	responses → responsesOutState：chat chunks → responses SSE 事件
//
// 特例（透传更保真，不走桥）：
//	入口 messages + 上游 anthropic、入口 responses + 上游 responses：
//	原始 SSE 直接透传（streamPassthrough + 通用 usage 提取）。

// sseMaxChunkLine SSE 单行上限（tool arguments 大块对齐请求体上限的一半）。
const sseMaxChunkLine = 4 << 20

// ---- 协议标识 ----

const (
	protoOpenAI    = "openai"
	protoAnthropic = "anthropic"
	protoResponses = "responses"
)

// ---- 通用工具 ----

// scanSSEData 扫描 SSE 流的 data 行（跳过 event:/注释/空行/[DONE]），回调 JSON payload。
func scanSSEData(r io.Reader, onData func(data json.RawMessage) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), sseMaxChunkLine)
	for sc.Scan() {
		payload, ok := parseSSELine(sc.Bytes())
		if !ok {
			continue
		}
		if err := onData(payload); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("sse read: %w", err)
	}
	return nil
}

// setupSSEHeaders 设置流式响应头（所有 SSE 入口共用）。
func setupSSEHeaders(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(status)
}

// writeAnthropicEvent 写一条 anthropic SSE 事件（event: + data: 双行）。
func writeAnthropicEvent(w io.Writer, evType string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte("event: " + evType + "\ndata: " + string(b) + "\n\n"))
	return err
}

// writeSSERaw 原样写一条 data 行（openai chunk 透传用）。
func writeSSERaw(w io.Writer, data []byte) error {
	_, err := w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
	return err
}

// intOr json 数值 → int（非数值返回 0）。
func intOr(v any) int { return intNum(v) }

// usageFromAny 从任意 usage map 提取记账（兼容 openai prompt_tokens/completion_tokens、
// anthropic/responses input_tokens/output_tokens 两套命名）。
func usageFromAny(v any) (UsageRecord, bool) {
	u, ok := v.(map[string]any)
	if !ok {
		return UsageRecord{}, false
	}
	p := intOr(u["prompt_tokens"])
	if p == 0 {
		p = intOr(u["input_tokens"])
	}
	c := intOr(u["completion_tokens"])
	if c == 0 {
		c = intOr(u["output_tokens"])
	}
	if p == 0 && c == 0 {
		return UsageRecord{}, false
	}
	return UsageRecord{PromptTokens: p, CompletionTokens: c, Found: true}, true
}

// errStopScan 提前终止扫描的哨兵（finish 已发出）。
var errStopScan = errors.New("stop scan")

// resolveStreamResult 统一桥接收尾：优先透出写客户端错误（errClientGone）；
// 上游读错误（流已开始，无法 failover）也透出；errStopScan 视为正常结束。
func resolveStreamResult(status int, lastUsage UsageRecord, streamErr, scanErr error) (int, UsageRecord, error) {
	if streamErr != nil {
		return status, lastUsage, streamErr
	}
	if scanErr != nil && !errors.Is(scanErr, errStopScan) && !errors.Is(scanErr, errClientGone) {
		return status, lastUsage, scanErr
	}
	if scanErr != nil && errors.Is(scanErr, errClientGone) {
		return status, lastUsage, errClientGone
	}
	return status, lastUsage, nil
}

// ---- 入口 messages 发射器：chat chunks → anthropic SSE ----

// anthropicOutState 把 chat chunk 流转成 anthropic SSE 事件序列
// （message_start → content_block_start/delta/stop → message_delta → message_stop）。
type anthropicOutState struct {
	id         string
	model      string
	started    bool
	blockIndex int         // 下一个 content block index
	curKind    string      // "" | "text" | "thinking" | "tool"
	curToolIdx int         // curKind=tool 时对应 chat tool index（-1 无）
	toolBlocks map[int]int // chat tool index → anthropic block index
	finish     string
	finishSent bool
	promptTok  int
	complTok   int
}

func newAnthropicOutState(model string) *anthropicOutState {
	return &anthropicOutState{
		id:         "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		model:      model,
		curToolIdx: -1,
		toolBlocks: map[int]int{},
		finish:     "stop",
	}
}

// messageStart 发流首事件。
func (s *anthropicOutState) messageStart(w io.Writer) error {
	if s.started {
		return nil
	}
	s.started = true
	return writeAnthropicEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      s.id,
			"type":    "message",
			"role":    "assistant",
			"model":   s.model,
			"content": []any{},
			"usage":   map[string]any{"input_tokens": s.promptTok, "output_tokens": 0},
		},
	})
}

// closeBlock 结束当前 content block（若有）。
func (s *anthropicOutState) closeBlock(w io.Writer) error {
	if s.curKind == "" {
		return nil
	}
	idx := s.blockIndex
	s.blockIndex++
	s.curKind = ""
	s.curToolIdx = -1
	return writeAnthropicEvent(w, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": idx,
	})
}

// openBlock 开启新 content block。
func (s *anthropicOutState) openBlock(w io.Writer, block map[string]any) error {
	idx := s.blockIndex
	return writeAnthropicEvent(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": block,
	})
}

// blockDelta 当前 block 的 delta。
func (s *anthropicOutState) blockDelta(w io.Writer, delta map[string]any) error {
	return writeAnthropicEvent(w, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.blockIndex, "delta": delta,
	})
}

// feed 消费一条 chat chunk，写出对应 anthropic 事件。usage 非 nil 表示捕获记账；
// done=true 表示终止序列已写完。
func (s *anthropicOutState) feed(w io.Writer, chunk map[string]any) (usage *UsageRecord, done bool, err error) {
	if m, _ := chunk["model"].(string); m != "" {
		s.model = m
	}
	if u, found := usageFromAny(chunk["usage"]); found {
		s.promptTok, s.complTok = u.PromptTokens, u.CompletionTokens
		usage = &u
	}

	choices, _ := chunk["choices"].([]any)
	var ch map[string]any
	if len(choices) > 0 {
		ch, _ = choices[0].(map[string]any)
	}
	if ch == nil {
		return usage, false, nil
	}

	frStr, _ := ch["finish_reason"].(string)
	if frStr != "" {
		s.finish = finishFromOpenAIFinish(frStr)
	}
	delta, _ := ch["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
	}

	if !s.started {
		if err := s.messageStart(w); err != nil {
			return usage, false, err
		}
	}

	if txt, ok := delta["content"].(string); ok && txt != "" {
		if s.curKind != "text" {
			if err := s.closeBlock(w); err != nil {
				return usage, false, err
			}
			s.curKind = "text"
			if err := s.openBlock(w, map[string]any{"type": "text", "text": ""}); err != nil {
				return usage, false, err
			}
		}
		if err := s.blockDelta(w, map[string]any{"type": "text_delta", "text": txt}); err != nil {
			return usage, false, err
		}
	}
	if th, ok := delta["reasoning_content"].(string); ok && th != "" {
		if s.curKind != "thinking" {
			if err := s.closeBlock(w); err != nil {
				return usage, false, err
			}
			s.curKind = "thinking"
			if err := s.openBlock(w, map[string]any{"type": "thinking", "thinking": ""}); err != nil {
				return usage, false, err
			}
		}
		if err := s.blockDelta(w, map[string]any{"type": "thinking_delta", "thinking": th}); err != nil {
			return usage, false, err
		}
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			idx := intOr(tc["index"])
			fn, _ := tc["function"].(map[string]any)
			if name, _ := fn["name"].(string); name != "" {
				// 新工具调用：关旧 block 开 tool_use block
				if err := s.closeBlock(w); err != nil {
					return usage, false, err
				}
				s.curKind = "tool"
				s.curToolIdx = idx
				s.toolBlocks[idx] = s.blockIndex
				if err := s.openBlock(w, map[string]any{
					"type":  "tool_use",
					"id":    firstNonEmpty(str(tc["id"]), sanitizeCallID(nil)),
					"name":  truncUTF8(name, 128),
					"input": map[string]any{},
				}); err != nil {
					return usage, false, err
				}
			}
			if args, _ := fn["arguments"].(string); args != "" {
				if err := s.blockDelta(w, map[string]any{"type": "input_json_delta", "partial_json": args}); err != nil {
					return usage, false, err
				}
			}
		}
	}

	if frStr != "" {
		if err := s.finishStream(w); err != nil {
			return usage, false, err
		}
		return usage, true, nil
	}
	return usage, false, nil
}

// finishStream 收尾：关 block → message_delta(stop_reason+usage) → message_stop。
func (s *anthropicOutState) finishStream(w io.Writer) error {
	if s.finishSent {
		return nil
	}
	s.finishSent = true
	if !s.started {
		// 无任何 delta 的流（极端）：也要发完整事件序列
		if err := s.messageStart(w); err != nil {
			return err
		}
	}
	if err := s.closeBlock(w); err != nil {
		return err
	}
	if err := writeAnthropicEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopFromFinish(s.finish), "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": s.promptTok, "output_tokens": s.complTok},
	}); err != nil {
		return err
	}
	return writeAnthropicEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

// finishFromOpenAIFinish chat finish_reason → 内部 finish（对齐 relayAgg 语义）。
func finishFromOpenAIFinish(fr string) string {
	switch fr {
	case "length":
		return "length"
	case "tool_calls", "function_call":
		return "tool_calls"
	default:
		return "stop"
	}
}

// ---- 入口 responses 发射器：chat chunks → responses SSE ----

// respOutTool 聚合中的 function_call item。
type respOutTool struct {
	itemID      string
	callID      string
	name        string
	arguments   string
	outputIndex int
}

// responsesOutState 把 chat chunk 流转成 responses SSE 事件序列。
type responsesOutState struct {
	respID  string
	created int64
	model   string
	started bool

	// message item（输出文本）
	msgOpen  bool
	msgID    string
	msgIndex int
	partOpen bool
	textBuf  string
	// reasoning item
	reasonOpen  bool
	reasonID    string
	reasonIndex int
	reasonBuf   string
	// function_call items
	tools     map[int]*respOutTool
	toolOrder []int

	itemIndex  int // 下一个 output_index
	finish     string
	finishSent bool
	promptTok  int
	complTok   int
	// output 聚合（response.completed 携带完整 output，按 output_index 排列）
	output map[int]any
}

func newResponsesOutState(model string) *responsesOutState {
	return &responsesOutState{
		respID:  "resp_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		created: time.Now().Unix(),
		model:   model,
		tools:   map[int]*respOutTool{},
		output:  map[int]any{},
		finish:  "stop",
	}
}

// responseBase responses 对象骨架（created/completed 事件共用）。
func (s *responsesOutState) responseBase() map[string]any {
	return map[string]any{
		"id":         s.respID,
		"object":     "response",
		"created_at": s.created,
		"status":     "in_progress",
		"model":      s.model,
		"output":     []any{},
		"usage":      nil,
	}
}

// nextItemIndex 在 output_item.added 时立即分配稳定索引。后续所有 delta/done
// 必须复用该索引；不能等到流结束再分配，否则文本、推理和并行工具会互相覆盖。
func (s *responsesOutState) nextItemIndex() int {
	idx := s.itemIndex
	s.itemIndex++
	return idx
}

// feed 消费一条 chat chunk。usage 非 nil 表示捕获记账。
func (s *responsesOutState) feed(w io.Writer, chunk map[string]any) (usage *UsageRecord, done bool, err error) {
	if m, _ := chunk["model"].(string); m != "" {
		s.model = m
	}
	if u, found := usageFromAny(chunk["usage"]); found {
		s.promptTok, s.complTok = u.PromptTokens, u.CompletionTokens
		usage = &u
	}

	choices, _ := chunk["choices"].([]any)
	var ch map[string]any
	if len(choices) > 0 {
		ch, _ = choices[0].(map[string]any)
	}
	if ch == nil {
		return usage, false, nil
	}
	frStr, _ := ch["finish_reason"].(string)
	if frStr != "" {
		s.finish = finishFromOpenAIFinish(frStr)
	}
	delta, _ := ch["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
	}

	if !s.started {
		s.started = true
		if err := writeResponsesEvent(w, map[string]any{
			"type": "response.created", "response": s.responseBase(),
		}); err != nil {
			return usage, false, err
		}
	}

	if txt, ok := delta["content"].(string); ok && txt != "" {
		if !s.msgOpen {
			s.msgOpen = true
			s.msgIndex = s.nextItemIndex()
			s.msgID = "msg_" + strconv.Itoa(s.msgIndex)
			if err := writeResponsesEvent(w, map[string]any{
				"type":         "response.output_item.added",
				"output_index": s.msgIndex,
				"item": map[string]any{
					"type": "message", "id": s.msgID, "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			}); err != nil {
				return usage, false, err
			}
			if err := writeResponsesEvent(w, map[string]any{
				"type":    "response.content_part.added",
				"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": ""},
			}); err != nil {
				return usage, false, err
			}
		}
		s.textBuf += txt
		if err := writeResponsesEvent(w, map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       s.msgID,
			"output_index":  s.msgIndex,
			"content_index": 0,
			"delta":         txt,
		}); err != nil {
			return usage, false, err
		}
	}
	if th, ok := delta["reasoning_content"].(string); ok && th != "" {
		if !s.reasonOpen {
			s.reasonOpen = true
			s.reasonIndex = s.nextItemIndex()
			s.reasonID = "rs_" + strconv.Itoa(s.reasonIndex)
			if err := writeResponsesEvent(w, map[string]any{
				"type":         "response.output_item.added",
				"output_index": s.reasonIndex,
				"item":         map[string]any{"type": "reasoning", "id": s.reasonID, "summary": []any{}},
			}); err != nil {
				return usage, false, err
			}
		}
		s.reasonBuf += th
		if err := writeResponsesEvent(w, map[string]any{
			"type":          "response.reasoning_summary_text.delta",
			"item_id":       s.reasonID,
			"output_index":  s.reasonIndex,
			"summary_index": 0,
			"delta":         th,
		}); err != nil {
			return usage, false, err
		}
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			idx := intOr(tc["index"])
			fn, _ := tc["function"].(map[string]any)
			t := s.tools[idx]
			if t == nil {
				t = &respOutTool{
					itemID:      "fc_" + strconv.Itoa(idx),
					callID:      firstNonEmpty(str(tc["id"]), sanitizeCallID(nil)),
					name:        truncUTF8(str(fn["name"]), 128),
					outputIndex: s.nextItemIndex(),
				}
				s.tools[idx] = t
				s.toolOrder = append(s.toolOrder, idx)
				if err := writeResponsesEvent(w, map[string]any{
					"type":         "response.output_item.added",
					"output_index": t.outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": t.itemID, "call_id": t.callID,
						"name": t.name, "arguments": "", "status": "in_progress",
					},
				}); err != nil {
					return usage, false, err
				}
			}
			if args, _ := fn["arguments"].(string); args != "" {
				t.arguments += args
				if err := writeResponsesEvent(w, map[string]any{
					"type":         "response.function_call_arguments.delta",
					"item_id":      t.itemID,
					"output_index": t.outputIndex,
					"delta":        args,
				}); err != nil {
					return usage, false, err
				}
			}
		}
	}

	if frStr != "" && !s.finishSent {
		if err := s.finishStream(w); err != nil {
			return usage, false, err
		}
		return usage, true, nil
	}
	return usage, false, nil
}

// finishStream 收尾：补齐 done 事件 + response.completed。
func (s *responsesOutState) finishStream(w io.Writer) error {
	if s.finishSent {
		return nil
	}
	s.finishSent = true
	if !s.started {
		s.started = true
		if err := writeResponsesEvent(w, map[string]any{
			"type": "response.created", "response": s.responseBase(),
		}); err != nil {
			return err
		}
	}

	if s.reasonOpen {
		item := map[string]any{
			"type": "reasoning", "id": s.reasonID,
			"summary": []any{map[string]any{"type": "summary_text", "text": s.reasonBuf}},
		}
		s.output[s.reasonIndex] = item
		if err := writeResponsesEvent(w, map[string]any{
			"type": "response.output_item.done", "output_index": s.reasonIndex, "item": item,
		}); err != nil {
			return err
		}
		s.reasonOpen = false
	}
	if s.msgOpen {
		item := map[string]any{
			"type": "message", "id": s.msgID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": s.textBuf}},
		}
		s.output[s.msgIndex] = item
		if err := writeResponsesEvent(w, map[string]any{
			"type": "response.output_text.done", "item_id": s.msgID,
			"output_index": s.msgIndex, "content_index": 0, "text": s.textBuf,
		}); err != nil {
			return err
		}
		if err := writeResponsesEvent(w, map[string]any{
			"type":    "response.content_part.done",
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": s.textBuf},
		}); err != nil {
			return err
		}
		if err := writeResponsesEvent(w, map[string]any{
			"type": "response.output_item.done", "output_index": s.msgIndex, "item": item,
		}); err != nil {
			return err
		}
		s.msgOpen = false
	}
	for _, idx := range s.toolOrder {
		t := s.tools[idx]
		item := map[string]any{
			"type": "function_call", "id": t.itemID, "call_id": t.callID,
			"name": t.name, "arguments": t.arguments, "status": "completed",
		}
		s.output[t.outputIndex] = item
		if err := writeResponsesEvent(w, map[string]any{
			"type": "response.function_call_arguments.done", "item_id": t.itemID,
			"output_index": t.outputIndex, "arguments": t.arguments,
		}); err != nil {
			return err
		}
		if err := writeResponsesEvent(w, map[string]any{
			"type": "response.output_item.done", "output_index": t.outputIndex, "item": item,
		}); err != nil {
			return err
		}
	}

	status := "completed"
	incomplete := any(nil)
	if s.finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	completedOutput := make([]any, 0, len(s.output))
	for idx := 0; idx < s.itemIndex; idx++ {
		if item, ok := s.output[idx]; ok {
			completedOutput = append(completedOutput, item)
		}
	}
	completed := s.responseBase()
	completed["status"] = status
	completed["output"] = completedOutput
	completed["incomplete_details"] = incomplete
	completed["usage"] = map[string]any{
		"input_tokens": s.promptTok, "output_tokens": s.complTok,
		"total_tokens": s.promptTok + s.complTok,
	}
	return writeResponsesEvent(w, map[string]any{"type": "response.completed", "response": completed})
}

// writeResponsesEvent 写一条 responses SSE 事件（data: 单行，type 在 payload 里）。
func writeResponsesEvent(w io.Writer, data map[string]any) error {
	if tw, ok := w.(*responsesToolWriter); ok {
		for _, event := range tw.bridge.events(data) {
			if err := writeSSEData(w, event); err != nil {
				return err
			}
		}
		return nil
	}
	return writeSSEData(w, data)
}

// ---- 上游 anthropic 消费器：anthropic SSE → chat chunks ----

// anthropicInState 把 anthropic SSE 事件流转成 chat chunk 流。
type anthropicInState struct {
	roleSent    bool
	toolIdx     int         // 下一个 chat tool index
	blockToTool map[int]int // anthropic block index → chat tool index
	lastToolIdx int
	finish      string
	finishSent  bool
	promptTok   int
	complTok    int
	model       string
	chunkID     string
	created     int64
}

func newAnthropicInState(model string) *anthropicInState {
	return &anthropicInState{
		blockToTool: map[int]int{},
		lastToolIdx: -1,
		finish:      "stop",
		model:       model,
		chunkID:     "chatcmpl-anthropic-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		created:     time.Now().Unix(),
	}
}

// chunkBase 组装 chat chunk 骨架。
func (s *anthropicInState) chunkBase(delta map[string]any, finish *string) map[string]any {
	return map[string]any{
		"id":      s.chunkID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": finish,
		}},
	}
}

// withRole 首个 delta 附带 role。
func (s *anthropicInState) withRole(delta map[string]any) map[string]any {
	if !s.roleSent {
		delta["role"] = "assistant"
		s.roleSent = true
	}
	return delta
}

// usage 汇总当前记账。
func (s *anthropicInState) usage() UsageRecord {
	if s.promptTok == 0 && s.complTok == 0 {
		return UsageRecord{}
	}
	return UsageRecord{PromptTokens: s.promptTok, CompletionTokens: s.complTok, Model: s.model, Found: true}
}

// feedAnthropicEvent 消费一条 anthropic SSE 事件，经 onChunk 产出 0..n 条 chat chunk。
func (s *anthropicInState) feedAnthropicEvent(onChunk func(map[string]any) error, typeName string, ev map[string]any) error {
	if m, _ := ev["model"].(string); m != "" {
		s.model = m
	}

	switch typeName {
	case "message_start":
		if msg, ok := ev["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				s.promptTok = intOr(u["input_tokens"])
			}
		}
		// 首个 role chunk（空 delta，仅声明角色）
		if !s.roleSent {
			s.roleSent = true
			if err := onChunk(s.chunkBase(map[string]any{"role": "assistant"}, nil)); err != nil {
				return err
			}
		}

	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		blockIdx := intOr(ev["index"])
		if str(block["type"]) == "tool_use" {
			idx := s.toolIdx
			s.toolIdx++
			s.blockToTool[blockIdx] = idx
			s.lastToolIdx = idx
			s.finish = "tool_calls"
			if err := onChunk(s.chunkBase(s.withRole(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": idx,
					"id":    firstNonEmpty(str(block["id"]), sanitizeCallID(nil)),
					"type":  "function",
					"function": map[string]any{
						"name":      truncUTF8(str(block["name"]), 128),
						"arguments": "",
					},
				}},
			}), nil)); err != nil {
				return err
			}
		}

	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		blockIdx := intOr(ev["index"])
		if delta == nil {
			return nil
		}
		switch str(delta["type"]) {
		case "text_delta":
			if t, _ := delta["text"].(string); t != "" {
				if err := onChunk(s.chunkBase(s.withRole(map[string]any{"content": t}), nil)); err != nil {
					return err
				}
			}
		case "thinking_delta":
			if t, _ := delta["thinking"].(string); t != "" {
				if err := onChunk(s.chunkBase(s.withRole(map[string]any{"reasoning_content": t}), nil)); err != nil {
					return err
				}
			}
		case "input_json_delta":
			if pj, _ := delta["partial_json"].(string); pj != "" {
				idx, ok := s.blockToTool[blockIdx]
				if !ok {
					idx = s.lastToolIdx
					if idx < 0 {
						idx = 0
					}
				}
				if err := onChunk(s.chunkBase(s.withRole(map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    idx,
						"function": map[string]any{"arguments": pj},
					}},
				}), nil)); err != nil {
					return err
				}
			}
		}

	case "message_delta":
		if d, ok := ev["delta"].(map[string]any); ok {
			if sr, _ := d["stop_reason"].(string); sr != "" {
				s.finish = finishFromAnthropicStop(sr)
			}
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			s.complTok = intOr(u["output_tokens"])
			if v := intOr(u["input_tokens"]); v > 0 {
				s.promptTok = v
			}
		}

	case "message_stop":
		if !s.finishSent {
			s.finishSent = true
			fr := s.finish
			if err := onChunk(s.chunkBase(map[string]any{}, &fr)); err != nil {
				return err
			}
		}

	case "error":
		if !s.finishSent {
			s.finishSent = true
			msg := "upstream error"
			if e, ok := ev["error"].(map[string]any); ok {
				if m, _ := e["message"].(string); m != "" {
					msg = m
				}
			}
			fr := "stop"
			if err := onChunk(s.chunkBase(s.withRole(map[string]any{"content": "[Error] " + msg}), &fr)); err != nil {
				return err
			}
		}
	}
	return nil
}

// finalize 流异常中断时补终止块（正常 message_stop 已发的场合为 no-op）。
func (s *anthropicInState) finalize(onChunk func(map[string]any) error) error {
	if s.finishSent {
		return nil
	}
	s.finishSent = true
	fr := s.finish
	return onChunk(s.chunkBase(map[string]any{}, &fr))
}

// ---- 桥接主函数 ----

// bridgeChatChunksToEntry 上游流（chat chunk SSE）→ 入口 SSE（messages/responses）。
// entry=chat 时透传（仅测试/兜底路径使用；openai 上游 + chat 入口走 streamPassthrough）。
func bridgeChatChunksToEntry(w http.ResponseWriter, upResp *http.Response, entry, upModel string) (int, UsageRecord, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, UsageRecord{}, errors.New("streaming unsupported by writer")
	}
	setupSSEHeaders(w, upResp.StatusCode)

	var lastUsage UsageRecord
	var streamErr error
	writeAndFlush := func(fn func() error) error {
		if err := fn(); err != nil {
			streamErr = errClientGone
			return errClientGone
		}
		flusher.Flush()
		return nil
	}

	switch entry {
	case protoAnthropic:
		st := newAnthropicOutState(upModel)
		err := scanSSEData(upResp.Body, func(data json.RawMessage) error {
			var chunk map[string]any
			if json.Unmarshal(data, &chunk) != nil {
				return nil
			}
			u, _, err := st.feed(w, chunk)
			if u != nil {
				lastUsage = *u
			}
			if err != nil {
				streamErr = err
				return err
			}
			flusher.Flush()
			return nil
		})
		_ = st.finishStream(w)
		flusher.Flush()
		return resolveStreamResult(upResp.StatusCode, lastUsage, streamErr, err)

	case protoResponses:
		st := newResponsesOutState(upModel)
		err := scanSSEData(upResp.Body, func(data json.RawMessage) error {
			var chunk map[string]any
			if json.Unmarshal(data, &chunk) != nil {
				return nil
			}
			u, _, err := st.feed(w, chunk)
			if u != nil {
				lastUsage = *u
			}
			if err != nil {
				streamErr = err
				return err
			}
			flusher.Flush()
			return nil
		})
		_ = st.finishStream(w)
		flusher.Flush()
		return resolveStreamResult(upResp.StatusCode, lastUsage, streamErr, err)

	default: // chat：原样透传 data 行
		err := scanSSEData(upResp.Body, func(data json.RawMessage) error {
			if u, found := extractChunkUsage(data); found {
				lastUsage = u
			}
			return writeAndFlush(func() error { return writeSSERaw(w, data) })
		})
		_ = writeSSEDone(w)
		flusher.Flush()
		return resolveStreamResult(upResp.StatusCode, lastUsage, streamErr, err)
	}
}

// bridgeAnthropicSSEToEntry 上游 anthropic SSE → 入口 SSE。
// entry=chat：anthropicInState 产 chat chunks 透传；
// entry=responses：anthropicInState 产 chat chunks → responsesOutState；
// entry=messages 走原始透传（streamPassthrough，不在本函数）。
func bridgeAnthropicSSEToEntry(w http.ResponseWriter, upResp *http.Response, entry, upModel string) (int, UsageRecord, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, UsageRecord{}, errors.New("streaming unsupported by writer")
	}
	setupSSEHeaders(w, upResp.StatusCode)

	in := newAnthropicInState(upModel)
	var lastUsage UsageRecord
	var streamErr error
	writeAndFlush := func(fn func() error) error {
		if err := fn(); err != nil {
			streamErr = errClientGone
			return errClientGone
		}
		flusher.Flush()
		return nil
	}
	emitChat := func(chunk map[string]any) error {
		return writeAndFlush(func() error { return writeSSEData(w, chunk) })
	}

	switch entry {
	case protoResponses:
		out := newResponsesOutState(upModel)
		err := scanAnthropicSSE(upResp.Body, func(typeName string, ev map[string]any) error {
			return in.feedAnthropicEvent(func(chunk map[string]any) error {
				u, _, ferr := out.feed(w, chunk)
				if u != nil {
					lastUsage = *u
				}
				if ferr != nil {
					streamErr = ferr
					return ferr
				}
				flusher.Flush()
				return nil
			}, typeName, ev)
		})
		_ = out.finishStream(w)
		flusher.Flush()
		if lastUsage.Found == false {
			if u := in.usage(); u.Found {
				lastUsage = u
			}
		}
		return resolveStreamResult(upResp.StatusCode, lastUsage, streamErr, err)

	default: // chat
		err := scanAnthropicSSE(upResp.Body, func(typeName string, ev map[string]any) error {
			return in.feedAnthropicEvent(emitChat, typeName, ev)
		})
		_ = in.finalize(emitChat)
		_ = writeSSEDone(w)
		flusher.Flush()
		if !lastUsage.Found {
			if u := in.usage(); u.Found {
				lastUsage = u
			}
		}
		return resolveStreamResult(upResp.StatusCode, lastUsage, streamErr, err)
	}
}

// bridgeCodexSSEToMessages 上游 responses SSE → 入口 messages。
// 复用 convertCodexEvent（responses 事件 → chat chunk）+ anthropicOutState（chunk → anthropic 事件）。
func bridgeCodexSSEToMessages(w http.ResponseWriter, upResp *http.Response, upModel string) (int, UsageRecord, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, UsageRecord{}, errors.New("streaming unsupported by writer")
	}
	setupSSEHeaders(w, upResp.StatusCode)

	cs := newCodexChunkState()
	out := newAnthropicOutState(upModel)
	var lastUsage UsageRecord
	var streamErr error

	err := parseCodexSSE(upResp.Body, func(typeName string, ev map[string]any) error {
		chunk, usage, fin := convertCodexEvent(cs, typeName, ev)
		if usage != nil {
			lastUsage = *usage
		}
		if chunk != nil {
			if _, _, ferr := out.feed(w, chunk); ferr != nil {
				streamErr = errClientGone
				return ferr
			}
			flusher.Flush()
		}
		if fin {
			if ferr := out.finishStream(w); ferr != nil {
				streamErr = ferr
				return ferr
			}
			flusher.Flush()
			return errStopScan
		}
		return nil
	})
	_ = out.finishStream(w)
	flusher.Flush()
	return resolveStreamResult(upResp.StatusCode, lastUsage, streamErr, err)
}

// scanAnthropicSSE 扫描 anthropic SSE（data 行携带 type 字段）。
func scanAnthropicSSE(r io.Reader, onEvent func(typeName string, ev map[string]any) error) error {
	return scanSSEData(r, func(data json.RawMessage) error {
		var ev map[string]any
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		typeName, _ := ev["type"].(string)
		if strings.TrimSpace(typeName) == "" {
			return nil
		}
		return onEvent(typeName, ev)
	})
}
