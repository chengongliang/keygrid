package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/workbuddy"
)

// workbuddy_runtime.go relay ↔ workbuddy 运行态的桥接辅助。
//
// 上游协议差异（Retry-After、业务错误码）在 relay 侧抽取，workbuddy 包只接收
// 结构化事件，避免把上游响应解析塞进业务包。

// applyWorkbuddyScrub 对 WorkBuddy 上游请求体执行脱敏（设计 §3.4）。
//
// 返回最终要发送给上游的 body。off 档原样返回（连解析都不做，热路径零开销）；
// 其余档位调 workbuddy.ScrubRequest，成功后把结果交给 observeScrub 留痕。
// 任何失败（当前实现不会返回 error，但签名保留）都退回原 body：宁可漏洗，
// 也不能让请求因为脱敏而发不出去。
func (h *Handler) applyWorkbuddyScrub(p *model.Provider, body []byte) []byte {
	if p == nil {
		return body
	}
	mode := h.workbuddyScrubMode(p)
	if mode == workbuddy.ScrubOff {
		return body
	}
	scrubbed, rep, err := workbuddy.ScrubRequest(mode, body)
	if err != nil {
		log.Printf("[workbuddy] scrub failed provider=%d mode=%s: %v (passthrough)", p.ID, mode, err)
		return body
	}
	h.observeScrub(p, mode, rep)
	return scrubbed
}

// scrubAuditEvent on 档每次改写的审计事件名（只记规则名与消息条数，绝不记内容）。
const scrubAuditEvent = "workbuddy.scrub"

// scrubShadowLogInterval shadow 档命中日志的按渠道最小间隔。
//
// shadow 的语义是"只统计不改写"，设计文档只要求统计上报。仓库里没有现成的
// 非错误统计落点：request_errors 是失败诊断（写进去会被面板误读成渠道故障）、
// usage_logs 是计费口径、audit_log 的契约是"每次改写写一条"（shadow 明确不
// 改写）。为它单造一套指标存储属于过度设计，因此采用按渠道节流的单行日志
// （只含 provider_id / 规则计数 / 消息条数，无任何内容）。节流是必需的：
// Codex CLI 这类客户端每条请求都带相同指纹，逐请求打日志会刷屏。
const scrubShadowLogInterval = 5 * time.Minute

// scrubShadowLog shadow 命中日志的节流状态（按 provider id 记上次打印时间）。
var scrubShadowLog struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

// observeScrub 处理一次脱敏结果，分两档：
//   - on：rep.Changed 时写 audit_log 事件 workbuddy.scrub（只有规则计数与消息条数）；
//   - shadow：rep.Total() > 0 时按渠道节流打一行日志，不写库。
//
// off / 无命中：什么都不做。
func (h *Handler) observeScrub(p *model.Provider, mode string, rep workbuddy.ScrubReport) {
	if p == nil {
		return
	}
	switch mode {
	case workbuddy.ScrubOn:
		if !rep.Changed || h.Op == nil {
			return
		}
		h.Op.CreateAuditLog(&model.AuditLog{
			UserID: p.UserID,
			Event:  scrubAuditEvent,
			// 只记 provider_id / 消息条数 / 规则名计数 —— 不落 content、不落 body。
			Detail:    fmt.Sprintf("provider_id=%d messages=%d rules=%s", p.ID, rep.Messages, scrubHitSummary(rep.Hits)),
			CreatedAt: h.now(),
		})
	case workbuddy.ScrubShadow:
		if rep.Total() == 0 {
			return
		}
		if throttleScrubShadowLog(p.ID, h.now()) {
			log.Printf("[workbuddy] scrub shadow hit provider_id=%d rules=%s messages=%d",
				p.ID, scrubHitSummary(rep.Hits), rep.Messages)
		}
	}
}

// scrubHitSummary 把规则命中计数拼成稳定字符串（键名排序，便于比对）：
// "claude_code_identity=1,error_code_11128=2"。空命中返回空串。
func scrubHitSummary(hits map[string]int) string {
	if len(hits) == 0 {
		return ""
	}
	names := make([]string, 0, len(hits))
	for k := range hits {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", k, hits[k]))
	}
	return strings.Join(parts, ",")
}

// throttleScrubShadowLog 按渠道限频：距上次打印不足 scrubShadowLogInterval 时返回 false。
func throttleScrubShadowLog(providerID int64, now time.Time) bool {
	scrubShadowLog.mu.Lock()
	defer scrubShadowLog.mu.Unlock()
	if scrubShadowLog.last == nil {
		scrubShadowLog.last = map[int64]time.Time{}
	}
	if last, ok := scrubShadowLog.last[providerID]; ok && now.Sub(last) < scrubShadowLogInterval {
		return false
	}
	scrubShadowLog.last[providerID] = now
	return true
}

// retryAfterFrom 取上游错误里的 Retry-After 退避值（0 = 未提供）。
func retryAfterFrom(err error) time.Duration {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	return 0
}

// upstreamCodeFromError 从上游错误体里提取业务错误码（如 WorkBuddy 的 6004 限额）。
//
// 只读数值型 code / error.code / status 三个字段；正文本身绝不落日志。
// 解析失败返回 0（"未知"），调用方只把它当作参考信号。
func upstreamCodeFromError(err error) int {
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Body == "" {
		return 0
	}
	var obj map[string]any
	if json.Unmarshal([]byte(ue.Body), &obj) != nil {
		return 0
	}
	if v, ok := jsonInt(obj["code"]); ok {
		return v
	}
	if inner, ok := obj["error"].(map[string]any); ok {
		if v, ok := jsonInt(inner["code"]); ok {
			return v
		}
	}
	if v, ok := jsonInt(obj["status"]); ok {
		return v
	}
	return 0
}

// jsonInt 把 JSON 数字/数字字符串转成 int（其余类型视为不可转换）。
func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		if out, err := strconv.Atoi(n); err == nil {
			return out, true
		}
	}
	return 0, false
}
