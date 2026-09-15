package model

import "time"

// QuotaSnapshot OAuth 渠道额度快照（openai codex /wham/usage 同步结果）。
// 与渠道 1:1；主动探测（后台任务/手动刷新）与被动观察（relay 转发响应的
// x-codex-* 头，cli-proxy-api 同款思路）共用一张表，后者按 provider_id 覆盖合并。
type QuotaSnapshot struct {
	ID int64 `gorm:"primaryKey" json:"id"`
	// ProviderID 唯一索引（1:1）。
	ProviderID int64 `gorm:"uniqueIndex;not null" json:"provider_id"`
	// UserID 冗余列：用户侧查询硬隔离（写入时从 provider 取，随 provider 更新）。
	UserID   int64  `gorm:"index;not null" json:"user_id"`
	Platform string `json:"platform"` // oauth provider key（openai）
	// Status ok=最近一次同步成功；error=最近一次同步失败（data 保留上次成功数据）。
	Status string `json:"status"`
	// Error 最近一次同步错误（status=error 时展示）；空 = 正常。
	Error string `json:"error,omitempty"`
	// Data 解析后的额度数据（JSON）。
	Data QuotaData `gorm:"serializer:json;type:jsonb" json:"data"`
	// Source 数据来源：probe=主动查询 /wham/usage；relay=转发响应头被动观察。
	Source string `json:"source"`
	// FetchedAt 上游观测时间（非落库时间）。
	FetchedAt time.Time `json:"fetched_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (QuotaSnapshot) TableName() string { return "quota_snapshots" }

// QuotaData 额度数据：/wham/usage 响应的归一化子集（字段语义对齐官方 codex CLI）。
// 双窗口约定：primary=长窗口（新版语义为周限额，604800s），secondary=短窗口
// （通常 5h=18000s）；窗口含义不硬编码，由 WindowLabel 按 window_seconds 近似打标。
type QuotaData struct {
	// PlanType 订阅计划（plus / pro / team / free ...）。
	PlanType string `json:"plan_type,omitempty"`
	// RateLimit 主额度（普通用量）。
	RateLimit *QuotaRateLimit `json:"rate_limit,omitempty"`
	// Credits 按量积分余额（credits / API 按量计费）。
	Credits *QuotaCredits `json:"credits,omitempty"`
	// ResetCreditsAvailable 限速重置积分剩余次数（消耗一次可立即清零当前限速窗口，
	// 即「重置次数」）。
	ResetCreditsAvailable *int `json:"reset_credits_available,omitempty"`
	// Additional 模型级附加限额（如 GPT-5.3-Codex-Spark 独立配额）。
	Additional []QuotaAdditional `json:"additional,omitempty"`
}

// QuotaRateLimit 单条限额：allowed/limit_reached + 双窗口。
type QuotaRateLimit struct {
	Allowed      bool         `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Primary      *QuotaWindow `json:"primary,omitempty"`
	Secondary    *QuotaWindow `json:"secondary,omitempty"`
}

// QuotaWindow 单个限速窗口。
type QuotaWindow struct {
	// UsedPercent 已用百分比 0-100（剩余 = 100 - used_percent）。
	UsedPercent float64 `json:"used_percent"`
	// WindowSeconds 窗口大小（秒）；0 = 上游未给出。
	WindowSeconds int64 `json:"window_seconds"`
	// WindowLabel 窗口可读标签：5h / daily / weekly / monthly / yearly；空 = 未知。
	WindowLabel string `json:"window_label"`
	// ResetAfterSec 距重置秒数（reset_at 的冗余表示，便于直接展示）。
	ResetAfterSec int64 `json:"reset_after_seconds"`
	// ResetAt 重置时刻 Unix 秒；0 = 未知（缺失时由 reset_after_seconds 推算）。
	ResetAt int64 `json:"reset_at"`
}

// QuotaCredits credits 余额状态。
type QuotaCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

// QuotaAdditional 模型级附加限额（additional_rate_limits 数组元素）。
type QuotaAdditional struct {
	LimitName string          `json:"limit_name"`
	RateLimit *QuotaRateLimit `json:"rate_limit,omitempty"`
}
