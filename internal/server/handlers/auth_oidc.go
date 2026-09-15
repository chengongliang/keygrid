package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oidc"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/golang-jwt/jwt/v5"
)

// auth_oidc.go Keycloak 兼容 OIDC SSO 登录（Authorization Code + PKCE）。
//
// 流程：
//  1. GET /api/auth/oidc/login    → 302 IdP /authorize（state+nonce+PKCE 存 oidc_states，5min 一次性）
//  2. GET /api/auth/oidc/callback → code 换 token → 校验 id_token（JWKS 签名/iss/aud/exp/nonce）
//     → 按 sub→email 匹配用户（无则 JIT 建号 role=user，oidc_sub 落库；邮箱已存在则绑定）
//     → 签发平台 JWT（与本地登录同构）→ 302 回前端 hash 路由
//  3. GET /api/auth/logout        → RP-Initiated Logout（302 跳 IdP 登出，可选 id_token_hint）
//
// 安全：state 消费即删（防重放）、nonce 校验（防 token 注入）、PKCE S256、
// id_token 校验 alg 白名单（拒 none/HS*）、redirect 参数仅允许 #/ 开头（防开放重定向）。

// oidcStateTTL 登录流程有效期。
const oidcStateTTL = 5 * time.Minute

// OidcHandler SSO 登录入口。OIDC 配置来自平台设置（系统设置页动态修改，参考 new-api），
// 未配置时所有端点自动降级；OIDC_* 环境变量仅作 DB 未配置时的兜底。
type OidcHandler struct {
	Op            *op.Op
	PublicBaseURL string
	JWTSecret     string
	// Env OIDC_* 环境变量兜底配置（platform_settings 未设置对应键时生效）。
	Env OidcEnvConfig
	// Settings 覆盖 kv 配置来源（nil = 从 Op 读 platform_settings）；测试注入用。
	Settings settingGetter

	// 按配置指纹缓存的 Client：设置保存后下一个请求自动重建（热生效，无需重启）。
	mu      sync.Mutex
	sig     string
	client  *oidc.Client
	display string
}

// resolve 读取生效配置并按指纹懒构建 oidc.Client；未启用/配置不全返回 nil。
func (h *OidcHandler) resolve() *oidc.Client {
	src := h.Settings
	if src == nil {
		src = asGetter(h.Op)
	}
	s := LoadOidcSettings(src, h.Env)
	sig := fmt.Sprintf("%t|%s|%s|%s|%s|%s",
		s.Enabled, s.Issuer, s.ClientID, s.ClientSecret, strings.Join(s.Scopes, " "), s.DisplayName)
	h.mu.Lock()
	defer h.mu.Unlock()
	if sig != h.sig {
		h.sig = sig
		h.display = s.DisplayName
		if s.Enabled && s.Issuer != "" && s.ClientID != "" && s.ClientSecret != "" {
			h.client = oidc.NewClient(s.Issuer, s.ClientID, s.ClientSecret, s.Scopes, nil)
		} else {
			h.client = nil
		}
	}
	return h.client
}

// displayName IdP 显示名（SSO 按钮文案用）。
func (h *OidcHandler) displayName() string {
	h.resolve()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.display
}

// ssoText 登录页 SSO 按钮文案："使用 {display_name} 登录"。
func (h *OidcHandler) ssoText() string {
	d := strings.TrimSpace(h.displayName())
	if d == "" {
		d = "企业账号"
	}
	return "使用 " + d + " 登录"
}

// Enabled OIDC 是否已配置启用。
func (h *OidcHandler) Enabled() bool { return h.resolve() != nil }

// callbackURL IdP 回调地址（必须与 IdP client 配置里的 redirect URI 一致）。
func (h *OidcHandler) callbackURL() string {
	return strings.TrimRight(h.PublicBaseURL, "/") + "/api/auth/oidc/callback"
}

// Login GET /api/auth/oidc/login → 302 IdP authorize。可选 ?next=#/xxx（登录后落地的 hash 路由）。
func (h *OidcHandler) Login(w http.ResponseWriter, r *http.Request) {
	cl := h.resolve()
	if cl == nil {
		resp.NotFound(w, "oidc login not enabled")
		return
	}
	ctx := r.Context()

	state := oidc.RandomBase64URL(32)
	nonce := oidc.RandomBase64URL(32)
	verifier := oidc.NewCodeVerifier()

	// next 仅允许站内 hash 路由（#/ 开头），防开放重定向
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "#/") {
		next = ""
	}
	st := &model.OidcState{
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		Redirect:     next,
		ExpiresAt:    time.Now().Add(oidcStateTTL),
	}
	if err := h.Op.CreateOidcState(st); err != nil {
		resp.Internal(w, "create sso state failed")
		return
	}

	authURL, err := cl.AuthorizeURL(ctx, h.callbackURL(), state, nonce, oidc.S256Challenge(verifier))
	if err != nil {
		resp.BadGateway(w, "oidc discovery failed: "+err.Error())
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback GET /api/auth/oidc/callback?code=&state= （IdP 302 回来）。
func (h *OidcHandler) Callback(w http.ResponseWriter, r *http.Request) {
	cl := h.resolve()
	if cl == nil {
		oidcFail(w, "SSO 未启用")
		return
	}
	ctx := r.Context()
	q := r.URL.Query()

	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		middleware.Audit(h.Op, 0, middleware.AuditEventOidcLoginFail, "idp error: "+e, middleware.ClientIP(r), r.UserAgent())
		oidcFail(w, "IdP 拒绝了授权请求（"+e+"）"+oidcDescSuffix(desc))
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		oidcFail(w, "回调缺少 code/state 参数")
		return
	}

	// state 一次性消费（取出即删，防重放）
	st, err := h.Op.ConsumeOidcState(state)
	if err != nil {
		oidcFail(w, "登录状态无效或已过期（5 分钟有效），请返回登录页重试")
		return
	}

	tr, err := cl.ExchangeCode(ctx, h.callbackURL(), code, st.CodeVerifier)
	if err != nil {
		middleware.Audit(h.Op, 0, middleware.AuditEventOidcLoginFail, "token exchange: "+err.Error(), middleware.ClientIP(r), r.UserAgent())
		oidcFail(w, "换取 token 失败，请重试")
		return
	}

	claims, err := cl.VerifyIDToken(ctx, tr.IDToken)
	if err != nil {
		middleware.Audit(h.Op, 0, middleware.AuditEventOidcLoginFail, "id_token verify: "+err.Error(), middleware.ClientIP(r), r.UserAgent())
		oidcFail(w, "id_token 校验失败，请重试")
		return
	}
	clm := oidc.ClaimsFromMap(claims)
	if clm.Subject == "" {
		oidcFail(w, "id_token 缺少 sub 声明")
		return
	}
	// nonce 校验：id_token 里的 nonce 必须等于 login 时存的（防 token 注入/混用）
	if clm.Nonce == "" || clm.Nonce != st.Nonce {
		middleware.Audit(h.Op, 0, middleware.AuditEventOidcLoginFail, "nonce mismatch", middleware.ClientIP(r), r.UserAgent())
		oidcFail(w, "nonce 校验失败，请重新发起登录")
		return
	}

	email := strings.ToLower(strings.TrimSpace(clm.Email))
	if email == "" || !strings.Contains(email, "@") {
		oidcFail(w, "IdP 未返回有效 email，无法登录（需要 openid email scope）")
		return
	}

	u, err := h.resolveUser(ctx, clm.Subject, email, clm.Name, clm.EmailVer)
	if err != nil {
		middleware.Audit(h.Op, 0, middleware.AuditEventOidcLoginFail, "resolve user: "+err.Error(), middleware.ClientIP(r), r.UserAgent())
		oidcFail(w, err.Error())
		return
	}
	if u.Status != "active" {
		oidcFail(w, "账号已被禁用，请联系管理员")
		return
	}
	// ADMIN_EMAIL 兜底：SSO JIT 建号晚于启动时的启动提升（启动时用户还不存在），
	// 登录时按 env 再补一次；覆盖 JIT 建号 / 邮箱绑定 / 已绑定三条路径。
	if adminEmail := os.Getenv("ADMIN_EMAIL"); adminEmail != "" &&
		strings.EqualFold(email, strings.TrimSpace(adminEmail)) && u.Role != "admin" {
		if _, err := h.Op.PromoteAdminByEmail(email); err == nil {
			u.Role = "admin"
			log.Printf("[oidc] promoted %s to admin (ADMIN_EMAIL)", email)
		}
	}

	token, err := h.issueToken(u)
	if err != nil {
		oidcFail(w, "签发会话失败")
		return
	}
	_ = h.Op.TouchLastLogin(u.ID)
	middleware.Audit(h.Op, u.ID, middleware.AuditEventOidcLoginOK, "sub="+clm.Subject, middleware.ClientIP(r), r.UserAgent())

	// 302 回前端 hash 路由：token 放 fragment（# 后），不会发到服务器/日志
	next := st.Redirect
	if !strings.HasPrefix(next, "#/") {
		next = "#/"
	}
	target := strings.TrimRight(h.PublicBaseURL, "/") + "/" + next + "?sso_token=" + url.QueryEscape(token)
	http.Redirect(w, r, target, http.StatusFound)
}

// resolveUser sub 精确匹配 → email 匹配（绑定 sub）→ JIT 建号。
// JIT 不受注册策略限制（IdP 已把关），但受禁用状态限制（callback 已判）。
// emailVerified：绑定已有本地账号（= 身份接管路径）必须 IdP 已验证邮箱
// （email_verified），否则任何 IdP 用户可改邮箱冒充任意本地账号（含 admin）。
func (h *OidcHandler) resolveUser(_ context.Context, sub, email, name string, emailVerified bool) (*model.User, error) {
	// 1) 已绑定过：直接登录
	if u, err := h.Op.GetUserByOidcSub(sub); err == nil {
		return u, nil
	}
	// 2) 邮箱已存在（本地注册过）→ 绑定 oidc_sub
	if u, err := h.Op.GetUserByEmail(email); err == nil {
		if !emailVerified {
			return nil, fmt.Errorf("IdP 未验证邮箱（email_verified），无法与已有账号绑定；请在 IdP 开启邮箱验证后重试")
		}
		if u.OidcSub != nil && *u.OidcSub != sub {
			return nil, fmt.Errorf("email 已绑定其他 SSO 身份")
		}
		if err := h.Op.BindOidcSub(u.ID, sub); err != nil {
			return nil, fmt.Errorf("绑定 SSO 身份失败")
		}
		u.OidcSub = &sub
		return u, nil
	}
	// 3) JIT 建号（role=user）
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0] // 默认取邮箱前缀
	}
	u := &model.User{
		Email:        email,
		Name:         name,
		PasswordHash: "", // OIDC-only：本地密码登录被拒
		Role:         "user",
		Status:       "active",
		OidcSub:      &sub,
	}
	if err := h.Op.CreateUser(u); err != nil {
		return nil, fmt.Errorf("SSO 自动建号失败（邮箱冲突）")
	}
	return u, nil
}

// issueToken 与本地登录同构的平台 JWT（SessionAuth 中间件直接兼容）。
func (h *OidcHandler) issueToken(u *model.User) (string, error) {
	claims := jwt.MapClaims{
		"sub":  fmt.Sprint(u.ID),
		"role": u.Role,
		"exp":  time.Now().Add(24 * time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.JWTSecret))
}

// Logout GET/POST /api/auth/logout —— RP-Initiated Logout：
// OIDC 启用且 IdP 支持 end_session 时 302 跳 IdP 登出（浏览器带 IdP 会话 cookie 即登出 SSO），
// 否则 302 回应用首页。可选 ?id_token_hint= 原样透传（更精确的 IdP 会话定位）。
// 注意：登出仅做重定向，不校验平台登录态（无敏感数据返回）。
func (h *OidcHandler) Logout(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(h.PublicBaseURL, "/") + "/"
	if cl := h.resolve(); cl != nil {
		if es := cl.EndSession(r.Context()); es != "" {
			q := url.Values{}
			q.Set("post_logout_redirect_uri", base)
			q.Set("client_id", cl.ClientID)
			if hint := r.URL.Query().Get("id_token_hint"); hint != "" {
				q.Set("id_token_hint", hint)
			}
			http.Redirect(w, r, es+"?"+q.Encode(), http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, base, http.StatusFound)
}

// Config GET /api/auth/config —— 登录页渲染 SSO 按钮/注册入口用（公开）。
// callback_url 供管理员配置 IdP 的 redirect URI；registration_policy 控制登录页是否显示注册入口。
func (h *OidcHandler) Config(w http.ResponseWriter, _ *http.Request) {
	rp, err := h.Op.GetSetting(model.SettingRegistrationPolicy)
	if err != nil || rp == "" {
		rp = model.DefaultRegistrationPolicy()
	}
	resp.Success(w, map[string]any{
		"oidc_enabled":        h.Enabled(),
		"sso_text":            h.ssoText(),
		"oidc_callback_url":   h.callbackURL(),
		"registration_policy": rp,
	})
}

// ---- helpers ----

// oidcFail 浏览器流程错误页（与 /oauth/done 落地页同风格）。
func oidcFail(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>SSO 登录失败</title></head>
<body style="background:#09090b;color:#e4e4e7;font-family:system-ui;display:flex;align-items:center;justify-content:center;height:100vh">
<div style="text-align:center;max-width:28rem"><h2>⛔ SSO 登录失败</h2><p style="color:#a1a1aa">` + htmlEsc(msg) + `</p><p><a href="/" style="color:#60a5fa">← 返回登录页</a></p></div>
</body></html>`))
}

func htmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

func oidcDescSuffix(desc string) string {
	if desc == "" {
		return ""
	}
	return "：" + htmlEsc(desc)
}
