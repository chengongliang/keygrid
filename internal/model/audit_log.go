package model

import "time"

// AuditLog 安全审计日志：只记事件元数据，不落任何密钥/明文。
// 记录：注册、登录成功/失败、签发 key、创建/删除渠道、OAuth 授权完成、key 删除。
type AuditLog struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	UserID    int64     `gorm:"index" json:"user_id"` // 0 = 未登录（如登录失败）
	Event     string    `gorm:"index;not null" json:"event"`
	Detail    string    `json:"detail"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }
