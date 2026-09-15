package handlers

// oidc_settings.go: OIDC SSO 平台设置（参考 new-api 的 OIDC 系统设置）。
//
// new-api 把 OIDC 配置放在系统设置里由管理员动态修改，保存即生效、无需重启；
// keygrid 原本只支持 OIDC_* 环境变量（启动时固定）。这里对齐 new-api 的做法：
//   - 配置项落 platform_settings kv（client_secret AES-GCM 加密存储），管理端 GET/PUT 读写
//   - OidcHandler 按配置指纹动态构建 oidc.Client，设置保存后下一个请求即热生效
//   - 平滑兼容存量部署：DB 未设置的键回退 OIDC_* 环境变量

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
)

// OidcEnvConfig 环境变量兜底配置（DB 未设置对应键时生效），来自 conf.Load()。
type OidcEnvConfig struct {
	Enabled      bool
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// OidcSettings 生效的 OIDC 运行时配置（DB 优先，env 兜底）。
type OidcSettings struct {
	Enabled      bool
	DisplayName  string
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// DefaultOidcScopes OIDC scopes 缺省值（new-api 亦为 "openid email profile"）。
var DefaultOidcScopes = []string{"openid", "email", "profile"}

// settingGetter kv 读取最小接口（*op.Op 天然满足；测试用 fake 注入）。
type settingGetter interface {
	GetSetting(key string) (string, error)
}

// LoadOidcSettings 解析生效配置：DB 有值用 DB，否则回退 env。
// enabled 的 DB 值（含显式 "0"）整体覆盖 env；secret 解密失败视为未设置。
func LoadOidcSettings(g settingGetter, env OidcEnvConfig) OidcSettings {
	s := OidcSettings{
		Enabled:      env.Enabled,
		Issuer:       env.Issuer,
		ClientID:     env.ClientID,
		ClientSecret: env.ClientSecret,
		Scopes:       env.Scopes,
	}
	if v, ok := getSetting(g, model.SettingOidcEnabled); ok {
		s.Enabled = v == "1"
	}
	if v, ok := getSetting(g, model.SettingOidcIssuer); ok && v != "" {
		s.Issuer = v
	}
	if v, ok := getSetting(g, model.SettingOidcClientID); ok && v != "" {
		s.ClientID = v
	}
	if v, ok := getSetting(g, model.SettingOidcClientSecret); ok && v != "" {
		if plain, err := decryptOidcSecret(v); err == nil {
			s.ClientSecret = plain
		} else {
			// 典型原因：MASTER_KEY 轮换。静默回退 env 会表现为 SSO 莫名失效，这里显式暴露
			log.Printf("[oidc] client_secret decrypt failed (key rotated?): %v; falling back to env", err)
		}
	}
	if v, ok := getSetting(g, model.SettingOidcScopes); ok && v != "" {
		if sc := ParseOidcScopes(v); len(sc) > 0 {
			s.Scopes = sc
		}
	}
	if v, ok := getSetting(g, model.SettingOidcDisplayName); ok && v != "" {
		s.DisplayName = v
	}
	if len(s.Scopes) == 0 {
		s.Scopes = append([]string(nil), DefaultOidcScopes...)
	}
	return s
}

// asGetter *op.Op → settingGetter。防御 typed-nil：nil Op 返回 nil 接口，
// 避免在接口里包一层类型化 nil 指针导致 g == nil 判断失效。
func asGetter(db *op.Op) settingGetter {
	if db == nil {
		return nil
	}
	return db
}

// getSetting 读 kv；不存在返回 ("", false)。忽略查询错误（等价未设置）；
// g 为 nil（无 DB，纯 env/测试场景）时视为未设置。
func getSetting(g settingGetter, key string) (string, bool) {
	if g == nil {
		return "", false
	}
	v, err := g.GetSetting(key)
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

// ParseOidcScopes 逗号/空格分隔 → 去重保序的 scope 列表。
func ParseOidcScopes(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// ValidateOidcSettings 校验待保存的 OIDC 配置（参考 new-api：启用前必须填齐 Client ID/Secret）。
// issuer/clientID/clientSecret 传 nil 表示本次不修改该字段（沿用 cur 的值）。
func ValidateOidcSettings(cur OidcSettings, enabled *bool, issuer, clientID, clientSecret, scopes *string) error {
	sc := ParseOidcScopes(orEmpty(scopes))
	if scopes != nil && len(sc) == 0 {
		return errors.New("scopes 不能为空（至少需要 openid 与 email）")
	}
	if issuer != nil && *issuer != "" {
		if err := validateIssuerURL(*issuer); err != nil {
			return err
		}
	}
	// 登录流程依赖 email claim 匹配/建号，缺 email scope 时每次登录都会失败
	if scopes != nil && len(sc) > 0 && (!contains(sc, "openid") || !contains(sc, "email")) {
		return errors.New("scopes 必须包含 openid 与 email（登录依赖 email claim 匹配账号）")
	}
	if enabled == nil || !*enabled {
		return nil
	}
	eff := cur
	if issuer != nil {
		eff.Issuer = *issuer
	}
	if clientID != nil {
		eff.ClientID = *clientID
	}
	if clientSecret != nil {
		eff.ClientSecret = *clientSecret
	}
	if strings.TrimSpace(eff.Issuer) == "" || strings.TrimSpace(eff.ClientID) == "" || strings.TrimSpace(eff.ClientSecret) == "" {
		return errors.New("无法启用 OIDC 登录，请先填入 WellKnown 地址、Client ID 与 Client Secret")
	}
	return nil
}

// validateIssuerURL issuer 必须是 http(s) URL（discovery 的 base）。
// 注意：与渠道 base_url（普通用户输入，强制拒私网）不同，这里不做私网拒绝 ——
// 企业 IdP（Keycloak 等）常部署在内网，且 OIDC 配置仅 admin 可写（信任级别
// 等同运维），强制公网会废掉内网 IdP 部署；仅校验 scheme/host 防明显误配。
func validateIssuerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("WellKnown 地址格式无效: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("WellKnown 地址必须是 http(s) URL")
	}
	if u.Host == "" {
		return errors.New("WellKnown 地址缺少 host")
	}
	return nil
}

// NormalizeOidcIssuer 规范化 issuer：去首尾空白与尾随 "/"。
func NormalizeOidcIssuer(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// ---- client_secret 加解密（AES-GCM，与渠道凭据同一主密钥）----

// encryptOidcSecret 明文 → base64(AES-GCM) 落库；空串原样返回（表示未设置）。
func encryptOidcSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	b, err := crypto.Encrypt([]byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// decryptOidcSecret base64(AES-GCM) → 明文。
func decryptOidcSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", err
	}
	plain, err := crypto.Decrypt(b)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func orEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
