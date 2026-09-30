package relay

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UpstreamError 上游非 2xx 响应（含截断后的正文摘要）。
//
// Error() 刻意不含正文：上游错误体可能回显请求内容（prompt 片段），
// 错误链/日志只保留状态码；Detail() 供返回给发起请求的调用方排查，不写日志、不落库。
type UpstreamError struct {
	Status int
	Body   string
	// ContentType 上游响应 Content-Type（仅用于把错误归类成可读摘要，不参与落库正文）
	ContentType string
	// RetryAfter 上游 Retry-After 头解析结果（0 = 未提供或不可解析）。
	// 仅用于限流退避与渠道冷却，不落库。
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %d", e.Status)
}

// Detail 上游响应摘要（已截断；可能含请求回显，勿写入日志或落库）。
func (e *UpstreamError) Detail() string { return e.Body }

// parseRetryAfter 解析 Retry-After 头（秒数或 HTTP 日期），失败返回 0。
//
// 只做退避参考值，不做上限裁剪（裁剪在 workbuddy 冷却判定处做）。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
