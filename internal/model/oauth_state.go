package model

import (
	"time"
)

// OAuthState 授权流程临时态（device_code 轮询 / pkce code_verifier 等）。
// state 一次性，5-10 分钟过期；Resolve 成功或过期后删除。
type OAuthState struct {
	State       string            `gorm:"primaryKey" json:"state"`
	UserID      int64             `gorm:"index;not null" json:"user_id"`
	ProviderKey string            `gorm:"not null" json:"provider_key"`           // kimi | openai | anthropic
	Temp        map[string]string `gorm:"serializer:json;type:jsonb" json:"temp"` // device_code / code_verifier / _deviceId / callback code
	ExpiresAt   time.Time         `gorm:"not null" json:"expires_at"`
	CreatedAt   time.Time         `json:"created_at"`
}

func (OAuthState) TableName() string { return "oauth_states" }
