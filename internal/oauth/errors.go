package oauth

import (
	"fmt"
)

// PendingError device_code 轮询中间态（authorization_pending / slow_down），
// 调用方据此继续轮询而非报错。
type PendingError struct {
	Reason string
}

func (e *PendingError) Error() string { return "oauth pending: " + e.Reason }

// RefreshError 刷新失败（含上游 OAuth error 码，供 classify 归类）。
type RefreshError struct {
	Body       string
	Status     int
	OAuthError string // invalid_grant / ...
}

// Error() 不含上游正文：错误体可能回显敏感信息，只保留状态码与 OAuth error 码，
// 供分类/日志使用；Detail() 供调试排查，勿写入日志或落库。
func (e *RefreshError) Error() string {
	return fmt.Sprintf("oauth refresh failed (status=%d error=%s)", e.Status, e.OAuthError)
}

// Detail 上游响应摘要（可能含回显，勿写入日志或落库）。
func (e *RefreshError) Detail() string { return e.Body }
