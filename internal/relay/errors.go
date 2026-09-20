package relay

import "fmt"

// UpstreamError 上游非 2xx 响应（含截断后的正文摘要）。
//
// Error() 刻意不含正文：上游错误体可能回显请求内容（prompt 片段），
// 错误链/日志只保留状态码；Detail() 供返回给发起请求的调用方排查，不写日志、不落库。
type UpstreamError struct {
	Status int
	Body   string
	// ContentType 上游响应 Content-Type（仅用于把错误归类成可读摘要，不参与落库正文）
	ContentType string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %d", e.Status)
}

// Detail 上游响应摘要（已截断；可能含请求回显，勿写入日志或落库）。
func (e *UpstreamError) Detail() string { return e.Body }
