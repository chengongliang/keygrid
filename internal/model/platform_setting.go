package model

import "time"

// PlatformSetting 平台级 kv 设置：公告 banner / 维护模式 / 注册策略。
// 只有 admin 可写；relay / 注册路径只读。
type PlatformSetting struct {
	Key       string    `gorm:"primaryKey;column:key" json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PlatformSetting) TableName() string { return "platform_settings" }

// 设置键名与取值约定
const (
	SettingAnnouncement       = "announcement"        // 公告文本；空 = 不展示
	SettingMaintenanceMode    = "maintenance_mode"    // "1" = 维护中，/v1 返回 503
	SettingRegistrationPolicy = "registration_policy" // open | invite | closed

	// OIDC SSO 系统设置（参考 new-api：管理员在系统设置页动态配置，保存即热生效无需重启）。
	// 未设置的键回退 OIDC_* 环境变量（兼容存量部署）。
	SettingOidcEnabled      = "oidc_enabled" // "1"/"0"；未设置 = 跟随 env
	SettingOidcIssuer       = "oidc_issuer"  // issuer URL（discovery /.well-known/openid-configuration）
	SettingOidcClientID     = "oidc_client_id"
	SettingOidcClientSecret = "oidc_client_secret" // AES-GCM 加密后 base64 落库
	SettingOidcScopes       = "oidc_scopes"        // 逗号/空格分隔；空 = openid email profile
	SettingOidcDisplayName  = "oidc_display_name"  // SSO 按钮文案中的 IdP 名称；空 = "企业账号"

	// 出口代理：管理员在系统设置统一配置（http/https/socks5），用户仅在渠道上勾选是否启用。
	// 空 = 未配置代理，勾选了代理的渠道创建/启用/转发时会被拒绝。
	SettingProxyURL = "proxy_url"
)

// 注册策略默认值：未设置时视为开放注册
func DefaultRegistrationPolicy() string { return "open" }
