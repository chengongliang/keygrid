package model

import (
	"time"
)

// OidcState OIDC SSO 流程临时态：
// login 时创建（state+nonce+PKCE verifier，5min TTL），callback 消费即删。
// 与 oauth_states（provider 授权用）分开建表，避免 user_id/provider_key 非空约束冲突。
type OidcState struct {
	State        string    `gorm:"primaryKey" json:"state"`
	Nonce        string    `gorm:"not null" json:"nonce"`
	CodeVerifier string    `gorm:"not null" json:"code_verifier"`
	Redirect     string    `json:"redirect"` // 登录成功后前端 hash 路由（防开放重定向：仅允许 #/ 开头）
	ExpiresAt    time.Time `gorm:"not null" json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
}

func (OidcState) TableName() string { return "oidc_states" }
