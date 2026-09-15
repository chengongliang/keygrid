package model

import (
	"time"
)

type UsageLog struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	UserID     int64  `gorm:"index;not null" json:"user_id"`
	ApiKeyID   int64  `gorm:"not null" json:"api_key_id"`
	ProviderID int64  `gorm:"not null" json:"provider_id"`
	Model      string `json:"model"`
	// BillingModel 计费归一化名（渠道 BillingMap 归一化后的标准名；空 = 同 model）。
	// 费用聚合按 COALESCE(billing_model, model) 分组，明细保留真实请求名。
	BillingModel     string `gorm:"default:''" json:"billing_model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	StatusCode       int    `json:"status_code"`
	LatencyMs        int    `json:"latency_ms"`
	// Cost 本次请求费用（USD，请求完成时价格快照；未定价模型为 0）。只记金额元数据，不落内容。
	Cost      float64   `gorm:"default:0" json:"cost"`
	CreatedAt time.Time `json:"created_at"`
}

func (UsageLog) TableName() string { return "usage_logs" }
