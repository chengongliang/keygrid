package model

import "time"

type ApiKey struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	UserID      int64  `gorm:"index;not null" json:"user_id"`
	Name        string `json:"name"`
	KeyHash     string `gorm:"uniqueIndex;not null" json:"-"`
	KeyEnc      []byte `json:"-"`            // AES-GCM 加密的 key 明文（支持"查看/复制"；MasterKey 丢失则不可恢复）
	Prefix      string `json:"prefix"`       // sk-xxxx 前缀展示用
	IPWhitelist string `json:"ip_whitelist"` // 逗号分隔 IP/CIDR，空 = 不限
	ModelLimit  string `json:"model_limit"`  // 逗号分隔模型名，空 = 不限
	// 额度（落库链路 / 硬拦截）：
	// QuotaLimit 上限 USD；0 = 不限。QuotaUsed 累计已消耗 USD，只由系统（UsageWriter flush）
	// 异步增量维护，接口不可直接改；提供"重置用量"操作清零。
	QuotaLimit float64    `gorm:"default:0" json:"quota_limit"`
	QuotaUsed  float64    `gorm:"default:0" json:"quota_used"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Enabled    bool       `gorm:"default:true" json:"enabled"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (ApiKey) TableName() string { return "api_keys" }
