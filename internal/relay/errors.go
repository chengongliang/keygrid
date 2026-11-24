package relay

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/chengongliang/keygrid/internal/server/resp"
)

// UpstreamError 上游 HTTP 或 SSE 错误（含正文摘要）。
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

// gatewayUpstreamError 按入口协议回传上游 error 对象，保留 message/code/type/param。
// 非结构化正文由调用方沿用网关诊断；这里只发送给本次请求的调用方，不写日志。
func gatewayUpstreamError(w http.ResponseWriter, entry string, upstream *UpstreamError) bool {
	var payload struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(upstream.Body), &payload) != nil || len(payload.Error) == 0 {
		return false
	}
	body := map[string]any{"error": payload.Error}
	if entry == protoAnthropic {
		body["type"] = "error"
	}
	resp.JSON(w, upstream.Status, body)
	return true
}
