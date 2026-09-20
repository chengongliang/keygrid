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
	// BreakerCheck 该渠道是否参与熔断检测。默认 false（不熔断）：只有一条渠道的
	// 用户宁可一直重试，也不希望渠道被临时拒绝 —— 熔断后没有备选可 failover，
	// 等于整段不可用。not null + default:false 让 AutoMigrate 给存量行补 false
	// （否则存量行为 NULL，扫描到 bool 字段会报错）。
	BreakerCheck bool `gorm:"not null;default:false" json:"breaker_check"`
	// UAMode 上游 User-Agent 策略：
	//   ""（默认）→ 透传客户端 UA（上游看到真实客户端：pi-agent / codex / claude）；
	//              Codex 渠道例外，保持固定 codex_cli_rs —— 上游强依赖该 UA。
	//   "custom" → 用渠道自定义的 UserAgent；
	//   "forward" → 强制透传客户端 UA（含 Codex 渠道）。
	UAMode string `gorm:"not null;default:''" json:"ua_mode"`
	// UserAgent UAMode=custom 时的 UA 值（禁 CR/LF，≤256）；其余模式忽略。
	UserAgent string `gorm:"not null;default:''" json:"user_agent"`
	// UseProxy 渠道上游请求是否走平台代理（管理员在系统设置统一配置 proxy_url，
	// 用户只能勾选是否启用；如 OpenAI 需代理才能访问）。
	UseProxy bool `json:"use_proxy"`
	// Authorized 展示用授权标记（gorm:"-" 不落库）：api_key 恒 true；oauth = 存在 active 凭据。
	// 未完成授权的 oauth 渠道前端显示"待授权"（List handler 填充；创建时 enabled=false）。
	Authorized bool      `gorm:"-" json:"authorized"`
	CreatedAt  time.Time `json:"created_at"`
}

func (Provider) TableName() string { return "providers" }
