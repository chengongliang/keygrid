package middleware

import (
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
)

// audit.go 审计事件记录：只记事件元数据，不落任何密钥/明文。

// Event names
const (
	AuditEventRegister     = "user.register"
	AuditEventLoginOK      = "user.login"
	AuditEventLoginFail    = "user.login_fail"
	AuditEventLoginRateHit = "user.login_rate_limited"
	// 自助修改密码（成功与失败均留痕，detail 区分失败原因）
	AuditEventPasswordChange = "user.password_change"
	AuditEventKeyCreate      = "apikey.create"
	AuditEventKeyUpdate      = "apikey.update"
	AuditEventKeyReveal      = "apikey.reveal"
	AuditEventKeyDelete      = "apikey.delete"
	AuditEventProviderCreate = "provider.create"
	AuditEventProviderUpdate = "provider.update"
	AuditEventProviderDelete = "provider.delete"
	AuditEventOAuthDone      = "provider.oauth_authorized"

	// admin.* 高敏操作留痕
	AuditEventAdminRoleChange  = "admin.role_change"
	AuditEventAdminUserDisable = "admin.user_disable"
	AuditEventAdminUserEnable  = "admin.user_enable"
	AuditEventAdminResetPass   = "admin.reset_password"
	AuditEventAdminSettings    = "admin.settings_update"
	AuditEventAdminInviteNew   = "admin.invitation_create"

	// 计费： 价格表变更留痕
	AuditEventAdminPriceUpsert = "admin.price_upsert"
	AuditEventAdminPriceDelete = "admin.price_delete"
	AuditEventAdminPriceSync   = "admin.price_sync"

	// 渠道健康：熔断手动重置留痕
	AuditEventAdminBreakerReset = "admin.provider_breaker_reset"
	// 用户自己重置自己渠道的熔断（自愈：换 key / 上游恢复后）
	AuditEventProviderBreakerReset = "provider.breaker_reset"

	// OIDC SSO 登录留痕
	AuditEventOidcLoginOK   = "user.oidc_login"
	AuditEventOidcLoginFail = "user.oidc_login_fail"
)

// Audit 记录一条审计事件。op 为 nil 时静默跳过（单测/无 DB 场景）。
func Audit(o *op.Op, userID int64, event, detail, ip, ua string) {
	if o == nil {
		return
	}
	o.CreateAuditLog(&model.AuditLog{
		UserID:    userID,
		Event:     event,
		Detail:    detail,
		IP:        ip,
		UserAgent: ua,
	})
}
