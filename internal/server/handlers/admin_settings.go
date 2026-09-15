package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// AdminSettingsHandler 平台设置：公告 banner / 维护模式 / 注册策略 + 邀请码。
// Oidc：OIDC SSO 配置（参考 new-api 系统设置，管理员页面动态修改、保存即热生效）。
type AdminSettingsHandler struct {
	Op            *op.Op
	OidcEnv       OidcEnvConfig // OIDC_* 环境变量兜底（platform_settings 未设置时生效）
	PublicBaseURL string        // OIDC 回调地址拼接用
}

// OidcSettingsView 管理端 OIDC 配置视图（client_secret 解密回显，new-api 同为明文回显）。
type OidcSettingsView struct {
	Enabled      bool   `json:"enabled"`
	DisplayName  string `json:"display_name"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Scopes       string `json:"scopes"`
	CallbackURL  string `json:"callback_url"`
}

func (h *AdminSettingsHandler) oidcView() OidcSettingsView {
	s := LoadOidcSettings(asGetter(h.Op), h.OidcEnv)
	return OidcSettingsView{
		Enabled:      s.Enabled,
		DisplayName:  s.DisplayName,
		Issuer:       s.Issuer,
		ClientID:     s.ClientID,
		ClientSecret: s.ClientSecret,
		Scopes:       strings.Join(s.Scopes, " "),
		CallbackURL:  strings.TrimRight(h.PublicBaseURL, "/") + "/api/auth/oidc/callback",
	}
}

type settingsResp struct {
	Announcement       string            `json:"announcement"`
	MaintenanceMode    bool              `json:"maintenance_mode"`
	RegistrationPolicy string            `json:"registration_policy"`
	ProxyURL           string            `json:"proxy_url"`
	Oidc               *OidcSettingsView `json:"oidc,omitempty"`
}

// GET /api/admin/settings
func (h *AdminSettingsHandler) Get(w http.ResponseWriter, _ *http.Request) {
	resp.Success(w, h.current())
}

func (h *AdminSettingsHandler) current() settingsResp {
	ann, _ := h.Op.GetSetting(model.SettingAnnouncement)
	mm, _ := h.Op.GetSetting(model.SettingMaintenanceMode)
	rp, err := h.Op.GetSetting(model.SettingRegistrationPolicy)
	if err != nil || rp == "" {
		rp = model.DefaultRegistrationPolicy()
	}
	proxyURL, _ := h.Op.GetSetting(model.SettingProxyURL)
	ov := h.oidcView()
	return settingsResp{
		Announcement:       ann,
		MaintenanceMode:    mm == "1",
		RegistrationPolicy: rp,
		ProxyURL:           proxyURL,
		Oidc:               &ov,
	}
}

// PUT /api/admin/settings  {announcement?, maintenance_mode?, registration_policy?, oidc?}
func (h *AdminSettingsHandler) Put(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Announcement       *string `json:"announcement"`
		MaintenanceMode    *bool   `json:"maintenance_mode"`
		RegistrationPolicy *string `json:"registration_policy"`
		ProxyURL           *string `json:"proxy_url"`
		Oidc               *struct {
			Enabled      *bool   `json:"enabled"`
			DisplayName  *string `json:"display_name"`
			Issuer       *string `json:"issuer"`
			ClientID     *string `json:"client_id"`
			ClientSecret *string `json:"client_secret"`
			Scopes       *string `json:"scopes"`
		} `json:"oidc"`
	}
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}

	// OIDC 先校验后落库：非法配置（如启用但缺 Client ID）直接 400，
	// 不留"announcement 已更新但 oidc 被拒"的部分保存状态。
	if req.Oidc != nil {
		if !h.saveOidc(w, r, req.Oidc) {
			return // 错误已写响应
		}
	}

	var changes []string
	if req.Announcement != nil {
		v := strings.TrimSpace(*req.Announcement)
		if len(v) > 2000 {
			resp.BadRequest(w, "announcement too long (max 2000)")
			return
		}
		if err := h.Op.SetSetting(model.SettingAnnouncement, v); err != nil {
			resp.Internal(w, "save setting failed")
			return
		}
		changes = append(changes, "announcement")
	}
	if req.MaintenanceMode != nil {
		v := "0"
		if *req.MaintenanceMode {
			v = "1"
		}
		if err := h.Op.SetSetting(model.SettingMaintenanceMode, v); err != nil {
			resp.Internal(w, "save setting failed")
			return
		}
		changes = append(changes, "maintenance_mode="+v)
	}
	if req.RegistrationPolicy != nil {
		p := strings.ToLower(strings.TrimSpace(*req.RegistrationPolicy))
		if p != "open" && p != "invite" && p != "closed" {
			resp.BadRequest(w, "registration_policy must be open, invite or closed")
			return
		}
		if err := h.Op.SetSetting(model.SettingRegistrationPolicy, p); err != nil {
			resp.Internal(w, "save setting failed")
			return
		}
		changes = append(changes, "registration_policy="+p)
	}
	if req.ProxyURL != nil {
		// 宽容归一化：127.0.0.1:7890 等无 scheme 写法自动补 http://，
		// 落库统一存完整 URL（转发读取时无需再猜类型）
		v := httpx.NormalizeProxyURL(*req.ProxyURL)
		if v != "" {
			if err := httpx.ValidateProxyURL(v); err != nil {
				resp.BadRequest(w, "代理地址格式不正确（示例：http://127.0.0.1:7890 或 socks5://127.0.0.1:1080）："+err.Error())
				return
			}
		}
		if err := h.Op.SetSetting(model.SettingProxyURL, v); err != nil {
			resp.Internal(w, "save setting failed")
			return
		}
		changes = append(changes, "proxy_url")
	}
	if req.Oidc != nil {
		changes = append(changes, "oidc settings")
	}
	if len(changes) == 0 {
		resp.BadRequest(w, "nothing to update")
		return
	}

	middleware.Audit(h.Op, middleware.UserID(r.Context()), middleware.AuditEventAdminSettings,
		strings.Join(changes, ","), middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, h.current())
}

// saveOidc 保存 OIDC 配置（部分更新：nil 字段不动）。成功返回 true；失败时已写响应。
// client_secret AES-GCM 加密落库（与渠道凭据同一主密钥），审计只记键名不记值。
func (h *AdminSettingsHandler) saveOidc(w http.ResponseWriter, r *http.Request, o *struct {
	Enabled      *bool   `json:"enabled"`
	DisplayName  *string `json:"display_name"`
	Issuer       *string `json:"issuer"`
	ClientID     *string `json:"client_id"`
	ClientSecret *string `json:"client_secret"`
	Scopes       *string `json:"scopes"`
}) bool {
	// 规范化输入
	if o.DisplayName != nil {
		v := strings.TrimSpace(*o.DisplayName)
		o.DisplayName = &v
	}
	if o.Issuer != nil {
		v := NormalizeOidcIssuer(*o.Issuer)
		o.Issuer = &v
	}
	if o.ClientID != nil {
		v := strings.TrimSpace(*o.ClientID)
		o.ClientID = &v
	}
	if o.ClientSecret != nil {
		v := strings.TrimSpace(*o.ClientSecret)
		o.ClientSecret = &v
	}
	if o.Scopes != nil {
		v := strings.TrimSpace(*o.Scopes)
		o.Scopes = &v
	}

	// 校验（启用时要求配置齐备，参考 new-api；未传字段沿用当前生效值）
	cur := LoadOidcSettings(asGetter(h.Op), h.OidcEnv)
	if err := ValidateOidcSettings(cur, o.Enabled, o.Issuer, o.ClientID, o.ClientSecret, o.Scopes); err != nil {
		resp.BadRequest(w, err.Error())
		return false
	}

	writes := []struct {
		key string
		val string
	}{}
	if o.Enabled != nil {
		v := "0"
		if *o.Enabled {
			v = "1"
		}
		writes = append(writes, struct {
			key string
			val string
		}{model.SettingOidcEnabled, v})
	}
	if o.DisplayName != nil {
		writes = append(writes, struct {
			key string
			val string
		}{model.SettingOidcDisplayName, *o.DisplayName})
	}
	if o.Issuer != nil {
		writes = append(writes, struct {
			key string
			val string
		}{model.SettingOidcIssuer, *o.Issuer})
	}
	if o.ClientID != nil {
		writes = append(writes, struct {
			key string
			val string
		}{model.SettingOidcClientID, *o.ClientID})
	}
	if o.ClientSecret != nil {
		stored, err := encryptOidcSecret(*o.ClientSecret)
		if err != nil {
			resp.Internal(w, "encrypt oidc client secret failed")
			return false
		}
		writes = append(writes, struct {
			key string
			val string
		}{model.SettingOidcClientSecret, stored})
	}
	if o.Scopes != nil {
		writes = append(writes, struct {
			key string
			val string
		}{model.SettingOidcScopes, strings.Join(ParseOidcScopes(*o.Scopes), " ")})
	}

	for _, wr := range writes {
		if err := h.Op.SetSetting(wr.key, wr.val); err != nil {
			resp.Internal(w, "save setting failed")
			return false
		}
	}
	return true
}

// POST /api/admin/invitations  → 生成邀请码（注册策略=invite 用）
func (h *AdminSettingsHandler) CreateInvitation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// 过期天数，默认 14
		ExpiresInDays int `json:"expires_in_days"`
	}
	if err := resp.Decode(r, &req); err != nil {
		// 空请求体也算默认值
		req.ExpiresInDays = 0
	}
	if req.ExpiresInDays <= 0 || req.ExpiresInDays > 365 {
		req.ExpiresInDays = 14
	}

	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		resp.Internal(w, "generate code failed")
		return
	}
	code := hex.EncodeToString(buf)

	inv, err := h.Op.CreateInvitation(code, middleware.UserID(r.Context()), time.Now().Add(time.Duration(req.ExpiresInDays)*24*time.Hour))
	if err != nil {
		resp.Internal(w, "save invitation failed")
		return
	}
	middleware.Audit(h.Op, inv.CreatedBy, middleware.AuditEventAdminInviteNew,
		"invitation "+code, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, inv)
}

// GET /api/admin/invitations
func (h *AdminSettingsHandler) ListInvitations(w http.ResponseWriter, _ *http.Request) {
	invs, err := h.Op.ListInvitations(50)
	if err != nil {
		resp.Internal(w, "list invitations failed")
		return
	}
	if invs == nil {
		invs = []model.Invitation{}
	}
	resp.Success(w, invs)
}
