package model

import "time"

// Provider 用户自己的供应商渠道。user_id 是 owner —— 硬隔离根基。
type Provider struct {
	ID       int64  `gorm:"primaryKey" json:"id"`
	UserID   int64  `gorm:"index;not null" json:"user_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`     // api_key | oauth
	Protocol string `json:"protocol"` // openai | anthropic | gemini
	BaseURL  string `json:"base_url"`
	// OAuthProvider 显式指定列名 oauth_provider：GORM 默认 NamingStrategy 会把驼峰缩写词
	// OAuth 拆成 o_auth_provider，与手写 SQL（op.ListRefreshDueCredentials）不一致（SQLSTATE 42703）。
	OAuthProvider string            `gorm:"column:oauth_provider" json:"oauth_provider,omitempty"` // kind=oauth: kimi | openai | ...
	ModelMap      map[string]string `gorm:"serializer:json;type:jsonb" json:"model_map,omitempty"` // 平台模型名->上游模型名
	// BillingMap 计费名映射（计费）：{请求模型名 → 计费标准名}。入口名未命中价格表时
	// 经此归一化再查价；映射目标必须是价格表里存在的精确模型名（防映射逃费）。与路由解耦。
	BillingMap map[string]string `gorm:"serializer:json;type:jsonb" json:"billing_map,omitempty"`
	Priority   int               `json:"priority"`
	// Enabled 无 default tag：GORM 会把带 default 的 bool 零值替换成默认值落库，
	// 导致 oauth 渠道创建时的 Enabled=false（禁用直到授权）失效。创建路径显式赋值。
	Enabled bool `json:"enabled"`
	// UseProxy 渠道上游请求是否走平台代理（管理员在系统设置统一配置 proxy_url，
	// 用户只能勾选是否启用；如 OpenAI 需代理才能访问）。
	UseProxy bool `json:"use_proxy"`
	// Authorized 展示用授权标记（gorm:"-" 不落库）：api_key 恒 true；oauth = 存在 active 凭据。
	// 未完成授权的 oauth 渠道前端显示"待授权"（List handler 填充；创建时 enabled=false）。
	Authorized bool      `gorm:"-" json:"authorized"`
	CreatedAt  time.Time `json:"created_at"`
}

func (Provider) TableName() string { return "providers" }
