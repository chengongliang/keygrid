package model

import "time"

// workbuddy.go WorkBuddy / CodeBuddy 渠道的运行态与自动化设置。
//
// 硬隔离（AGENTS.md 规则 1）：三张表都带 user_id，所有查询必须带
// `WHERE user_id = ?`，且只经由 op 层访问。
//
// 设计见 docs/workbuddy-integration-design.md §2.4。表名刻意带 workbuddy 前缀：
// 这套结构是供应商特化，不是通用插件模型 —— 名字要诚实反映这一点。

// WorkbuddySetting 用户级设置（用户一份，与渠道数无关）。
type WorkbuddySetting struct {
	ID     int64 `gorm:"primaryKey" json:"id"`
	UserID int64 `gorm:"uniqueIndex;not null" json:"user_id"`
	// Enabled 用户在该供应商上的总开关（默认关：会伪装客户端行为，必须显式开启）。
	Enabled bool `gorm:"not null;default:false" json:"enabled"`
	// ReserveCredits 保留积分：积分低于该值时不再使用该渠道（0 = 不保留）。
	ReserveCredits float64 `gorm:"not null;default:0" json:"reserve_credits"`
	// DailyTokenLimit 该供应商每日 token 上限，按渠道分别判定（0 = 不限）。
	DailyTokenLimit int64 `gorm:"not null;default:0" json:"daily_token_limit"`
	// Product 出站身分档位：workbuddy（桌面端，默认）/ vscode / cli。
	Product string `gorm:"size:16;not null;default:'workbuddy'" json:"product"`
	// ScrubMode WAF 指纹脱敏档位：off（默认）/ shadow（只统计）/ on（改写）。
	ScrubMode string `gorm:"size:8;not null;default:'off'" json:"scrub_mode"`
	// Fingerprint 是否附带设备指纹头（默认关）。
	Fingerprint bool `gorm:"not null;default:false" json:"fingerprint"`
	// JobsEnabled 逐任务开关：{checkin:true, growth:false, ...}，全部默认关。
	JobsEnabled map[string]bool `gorm:"serializer:json;type:jsonb" json:"jobs_enabled,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func (WorkbuddySetting) TableName() string { return "workbuddy_settings" }

// WorkbuddyChannelState 渠道运行态：一个 provider（= 一个上游账号）一行。
type WorkbuddyChannelState struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	UserID     int64  `gorm:"index;not null" json:"user_id"`
	ProviderID int64  `gorm:"uniqueIndex;not null" json:"provider_id"`
	Realm      string `gorm:"size:8" json:"realm"` // intl | cn
	// AccountUID 账号标识，只存前缀用于展示与派生校验（完整 uid 从凭据的
	// TokenSet.Extra 现取，不再造一份可关联的明文副本）。
	AccountUID string `gorm:"size:32" json:"account_uid"`
	Nickname   string `gorm:"size:128" json:"nickname"`
	// Credits 最近一次观测到的积分余额；CreditsAt 是观测时间。
	Credits   float64   `json:"credits"`
	CreditsAt time.Time `json:"credits_at"`
	// DailyTokens / TokensDate 当日用量缓存（权威值仍来自 usage_logs 聚合，
	// 这里只用于面板展示与热路径判断）。
	DailyTokens int64  `gorm:"not null;default:0" json:"daily_tokens"`
	TokensDate  string `gorm:"size:10" json:"tokens_date"` // YYYY-MM-DD（用户本地时区）
	// ParkedReason 当前停用原因："" | disabled | daily_limit | reserve_credits | cooldown。
	ParkedReason string `gorm:"size:32" json:"parked_reason"`
	// Cooldowns 模型级冷却：{模型名: 恢复时间}（上游 429 / 6004 限额）。
	Cooldowns map[string]time.Time `gorm:"serializer:json;type:jsonb" json:"cooldowns,omitempty"`
	LastOKAt  *time.Time           `json:"last_ok_at,omitempty"`
	// Meta 幂等与去重标记：lastCheckin / lastGrowth / lastNight / lastDailyChat /
	// GrowthEventIDs（成长任务事件去重）等。只放非敏感元数据。
	Meta      map[string]any `gorm:"serializer:json;type:jsonb" json:"meta,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func (WorkbuddyChannelState) TableName() string { return "workbuddy_channel_state" }

// WorkbuddyJobRun 任务执行日志（面板展示 + 审计）。
//
// Message 只允许放状态摘要（状态码/上游错误码/输出段数/耗时），
// 禁止放 prompt、响应正文或任何凭据 —— 与 request_errors 同一红线。
type WorkbuddyJobRun struct {
	ID         int64      `gorm:"primaryKey" json:"id"`
	UserID     int64      `gorm:"index;not null" json:"user_id"`
	JobKey     string     `gorm:"size:48;not null;index" json:"job_key"`
	ProviderID *int64     `gorm:"index" json:"provider_id,omitempty"`
	Trigger    string     `gorm:"size:16" json:"trigger"` // schedule | manual
	Status     string     `gorm:"size:16" json:"status"`  // ok | skipped | failed | parked
	Message    string     `gorm:"size:512" json:"message"`
	StartedAt  time.Time  `gorm:"index" json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func (WorkbuddyJobRun) TableName() string { return "workbuddy_job_runs" }
