package model

import "time"

// RequestError 单次失败转发请求的诊断记录（渠道健康页排查用）。
//
// 与 usage_logs 分开存：usage_logs 是「每次请求一行」的统计口径（成功与失败都要），
// 而这里是可过期的诊断数据（默认保留 7 天，后台任务清理），失败量大时不会把
// 统计表撑大、也不会拖慢用量聚合。
//
// 只记元数据与上游错误消息摘要：绝不落 prompt / 响应正文（AGENTS 硬性规则 4）。
// Message 在写入前统一截断（见 relay.sanitizeErrMessage）。
type RequestError struct {
	ID     int64 `gorm:"primaryKey" json:"id"`
	UserID int64 `gorm:"index:idx_reqerr_user_time,priority:1;not null" json:"user_id"`
	// ProviderID 0 = 路由层失败（没有可用候选渠道），此时不存在具体渠道
	ProviderID int64  `gorm:"index:idx_reqerr_provider_time,priority:1" json:"provider_id"`
	ApiKeyID   int64  `json:"api_key_id"`
	Model      string `json:"model"`
	// Kind 失败分类，取值见 relay 的 errKind* 常量：
	// no_channel / circuit_open / credential / transform / proxy /
	// upstream_4xx / upstream_429 / upstream_5xx / transport / stream_aborted
	Kind       string    `json:"kind"`
	StatusCode int       `json:"status_code"`
	Message    string    `json:"message"` // 上游错误摘要，截断到 512 字符
	LatencyMs  int       `json:"latency_ms"`
	CreatedAt  time.Time `gorm:"index:idx_reqerr_user_time,priority:2;index:idx_reqerr_provider_time,priority:2;index:idx_reqerr_cleanup" json:"created_at"`
}

func (RequestError) TableName() string { return "request_errors" }
