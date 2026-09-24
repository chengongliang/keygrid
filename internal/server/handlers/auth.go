package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	Op        *op.Op
	JWTSecret string
}

type registerReq struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	Password   string `json:"password"`
	InviteCode string `json:"invite_code"` // 注册策略=invite 时必填
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(req.Email, "@") {
		resp.BadRequest(w, "valid email required")
		return
	}
	if len(req.Password) < 8 {
		resp.BadRequest(w, "password must be at least 8 characters")
		return
	}

	// 注册策略（open | invite | closed）。首个用户（空库）不受限制。
	var userCount int64
	_ = h.Op.DB.Model(&model.User{}).Count(&userCount).Error
	if userCount > 0 {
		policy, err := h.Op.GetSetting(model.SettingRegistrationPolicy)
		if err == nil && policy == "" {
			policy = model.DefaultRegistrationPolicy()
		}
		switch policy {
		case "closed":
			resp.Error(w, http.StatusForbidden, 403, "registration is closed")
			return
		case "invite":
			if strings.TrimSpace(req.InviteCode) == "" {
				resp.Error(w, http.StatusForbidden, 403, "invite code required")
				return
			}
			// 先校验邀请码有效，再建号（消费在事务外，见下方）
			if _, err := h.Op.GetUserByEmail(req.Email); err == nil {
				resp.BadRequest(w, "email already registered")
				return
			}
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		resp.Internal(w, "hash password failed")
		return
	}
	u := &model.User{
		Email:        req.Email,
		Name:         req.Name,
		PasswordHash: string(hash),
		Role:         "user",
		Status:       "active",
	}
	if err := h.Op.CreateUser(u); err != nil {
		resp.BadRequest(w, "email already registered")
		return
	}
	// 邀请码在成功建号后消费（失败则回滚删除账号，保证码不被浪费）
	if userCount > 0 {
		policy, _ := h.Op.GetSetting(model.SettingRegistrationPolicy)
		if policy == "invite" {
			if err := h.Op.ConsumeInvitation(strings.TrimSpace(req.InviteCode), u.ID); err != nil {
				_ = h.Op.DB.Delete(u).Error
				resp.Error(w, http.StatusForbidden, 403, "invalid or expired invite code")
				return
			}
		}
	}
	middleware.Audit(h.Op, u.ID, middleware.AuditEventRegister, u.Email, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"id": u.ID, "email": u.Email})
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	u, err := h.Op.GetUserByEmail(strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil {
		middleware.Audit(h.Op, 0, middleware.AuditEventLoginFail, "user not found: "+req.Email, middleware.ClientIP(r), r.UserAgent())
		resp.Unauthorized(w, "invalid email or password")
		return
	}
	// OIDC-only 用户（password_hash 为空）不允许本地密码登录
	if u.PasswordHash == "" {
		middleware.Audit(h.Op, u.ID, middleware.AuditEventLoginFail, "oidc-only user tried password login", middleware.ClientIP(r), r.UserAgent())
		resp.Unauthorized(w, "该账号为 SSO 登录账号，请使用「企业账号登录」")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		middleware.Audit(h.Op, u.ID, middleware.AuditEventLoginFail, "wrong password", middleware.ClientIP(r), r.UserAgent())
		resp.Unauthorized(w, "invalid email or password")
		return
	}
	if u.Status != "active" {
		resp.Unauthorized(w, "user disabled")
		return
	}
	token, err := h.issueToken(u)
	if err != nil {
		resp.Internal(w, "issue token failed")
		return
	}
	_ = h.Op.TouchLastLogin(u.ID)
	middleware.Audit(h.Op, u.ID, middleware.AuditEventLoginOK, "", middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{
		"token": token,
		"user":  sessionUser(u),
	})
}

// ChangePassword POST /api/auth/change_password —— 自助修改密码（需验证旧密码）。
// SSO-only 账号（password_hash 为空，账号由 OIDC JIT 建号）禁用：密码由 IdP 管理，
// 平台侧不存在可改的密码，handler 层直接拒绝（前端依据 has_password=false 隐藏表单）。
func (h *AuthHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	uid := middleware.UserID(r.Context())
	u, err := h.Op.GetUser(uid)
	if err != nil {
		resp.NotFound(w, "user not found")
		return
	}
	if status, msg, detail := checkPasswordChange(u.PasswordHash, req.OldPassword, req.NewPassword); status != 0 {
		if detail != "" { // 只留痕安全相关失败（旧密码错误 / SSO-only 尝试），强度不足属表单校验
			middleware.Audit(h.Op, uid, middleware.AuditEventPasswordChange, detail, middleware.ClientIP(r), r.UserAgent())
		}
		resp.Error(w, status, status, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		resp.Internal(w, "hash password failed")
		return
	}
	if err := h.Op.SetPassword(uid, string(hash)); err != nil {
		resp.Internal(w, "update password failed")
		return
	}
	middleware.Audit(h.Op, uid, middleware.AuditEventPasswordChange, "changed own password", middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"ok": true})
}

// checkPasswordChange 自助改密的纯校验逻辑（单测覆盖），返回非 0 状态码即拒绝；
// detail 仅用于审计留痕（空 = 无需留痕的普通表单校验失败）。
func checkPasswordChange(passwordHash, oldPass, newPass string) (status int, msg, detail string) {
	if passwordHash == "" {
		return http.StatusForbidden, "password is managed by SSO provider", "oidc-only user tried password change"
	}
	if len(newPass) < 8 {
		return http.StatusBadRequest, "new password must be at least 8 characters", ""
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(oldPass)) != nil {
		return http.StatusBadRequest, "current password is incorrect", "wrong current password"
	}
	return 0, "", ""
}

// Me GET /api/auth/me —— 前端会话校验/用户信息。
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	u, err := h.Op.GetUser(middleware.UserID(r.Context()))
	if err != nil {
		resp.NotFound(w, "user not found")
		return
	}
	resp.Success(w, sessionUser(u))
}

// sessionUser 会话用户信息。has_password=false 表示 OIDC-only 账号（无本地密码），
// 前端据此禁用「修改密码」入口。
func sessionUser(u *model.User) map[string]any {
	return map[string]any{
		"id":           u.ID,
		"email":        u.Email,
		"name":         u.Name,
		"role":         u.Role,
		"has_password": u.PasswordHash != "",
	}
}

func (h *AuthHandler) issueToken(u *model.User) (string, error) {
	claims := jwt.MapClaims{
		"sub":  fmt.Sprint(u.ID),
		"role": u.Role,
		"exp":  time.Now().Add(24 * time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.JWTSecret))
}
