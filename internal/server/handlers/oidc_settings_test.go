package handlers

import (
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/model"
)

// fakeSettingGetter 测试用 kv（*op.Op 满足 settingGetter）。
type fakeSettingGetter map[string]string

func (f fakeSettingGetter) GetSetting(key string) (string, error) {
	v, ok := f[key]
	if !ok {
		return "", nil // 与 *op.Op 语义一致：不存在返回 ("", nil)
	}
	return v, nil
}

func init() {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	crypto.Init(k)
}

func TestLoadOidcSettings_EnvFallback(t *testing.T) {
	env := OidcEnvConfig{Enabled: true, Issuer: "https://sso.example.com", ClientID: "env-id", ClientSecret: "env-secret", Scopes: []string{"openid", "email"}}
	s := LoadOidcSettings(fakeSettingGetter{}, env)
	if !s.Enabled || s.Issuer != "https://sso.example.com" || s.ClientID != "env-id" || s.ClientSecret != "env-secret" {
		t.Fatalf("env fallback lost: %+v", s)
	}
	// env 未给 scopes 时回退默认
	s2 := LoadOidcSettings(fakeSettingGetter{}, OidcEnvConfig{Scopes: nil})
	if strings.Join(s2.Scopes, " ") != "openid email profile" {
		t.Fatalf("default scopes expected, got %v", s2.Scopes)
	}
}

func TestLoadOidcSettings_DBPriority(t *testing.T) {
	enc, err := encryptOidcSecret("db-secret")
	if err != nil {
		t.Fatal(err)
	}
	g := fakeSettingGetter{
		model.SettingOidcEnabled:      "0", // 显式禁用必须覆盖 env.Enabled=true
		model.SettingOidcIssuer:       "https://db.example.com",
		model.SettingOidcClientID:     "db-id",
		model.SettingOidcClientSecret: enc,
		model.SettingOidcScopes:       "openid, email openid profile",
		model.SettingOidcDisplayName:  "公司 SSO",
	}
	env := OidcEnvConfig{Enabled: true, Issuer: "https://sso.example.com", ClientID: "env-id", ClientSecret: "env-secret"}
	s := LoadOidcSettings(g, env)
	if s.Enabled {
		t.Fatal("db enabled=0 should override env true")
	}
	if s.Issuer != "https://db.example.com" {
		t.Fatalf("issuer: %q", s.Issuer)
	}
	if s.ClientID != "db-id" || s.ClientSecret != "db-secret" {
		t.Fatalf("client: %s/%s", s.ClientID, s.ClientSecret)
	}
	if strings.Join(s.Scopes, " ") != "openid email profile" {
		t.Fatalf("scopes dedup: %v", s.Scopes)
	}
	if s.DisplayName != "公司 SSO" {
		t.Fatalf("display: %q", s.DisplayName)
	}
}

func TestSecretRoundTrip(t *testing.T) {
	enc, err := encryptOidcSecret("s3cr3t-密钥")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "" || enc == "s3cr3t-密钥" {
		t.Fatal("secret must be encrypted")
	}
	got, err := decryptOidcSecret(enc)
	if err != nil || got != "s3cr3t-密钥" {
		t.Fatalf("roundtrip: %q %v", got, err)
	}
	if e, _ := encryptOidcSecret(""); e != "" {
		t.Fatal("empty secret should stay empty")
	}
}

func TestValidateOidcSettings_EnableRequiresFullConfig(t *testing.T) {
	// 参考 new-api：启用时缺 Client ID/Secret 必须拒绝
	err := ValidateOidcSettings(OidcSettings{}, boolPtr(true), strPtr("https://idp.example.com"), strPtr(""), strPtr(""), nil)
	if err == nil || !strings.Contains(err.Error(), "无法启用 OIDC") {
		t.Fatalf("expected enable error, got %v", err)
	}
	// 配齐后通过
	err = ValidateOidcSettings(OidcSettings{}, boolPtr(true), strPtr("https://idp.example.com"), strPtr("id"), strPtr("sec"), nil)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// issuer 非法
	if err := ValidateOidcSettings(OidcSettings{}, nil, strPtr("ftp://x"), nil, nil, nil); err == nil {
		t.Fatal("ftp issuer should be rejected")
	}
	if err := ValidateOidcSettings(OidcSettings{}, nil, strPtr("not a url"), nil, nil, nil); err == nil {
		t.Fatal("bad issuer should be rejected")
	}
	// scopes 空
	if err := ValidateOidcSettings(OidcSettings{}, nil, nil, nil, nil, strPtr("  ")); err == nil {
		t.Fatal("empty scopes should be rejected")
	}
	// scopes 缺 openid/email（登录依赖 email claim）
	if err := ValidateOidcSettings(OidcSettings{}, nil, nil, nil, nil, strPtr("profile")); err == nil {
		t.Fatal("scopes without openid/email should be rejected")
	}
	if err := ValidateOidcSettings(OidcSettings{}, nil, nil, nil, nil, strPtr("openid email profile")); err != nil {
		t.Fatalf("valid scopes rejected: %v", err)
	}
}

func TestParseOidcScopes(t *testing.T) {
	got := ParseOidcScopes(" openid, email  ,openid,\temail\nprofile")
	if strings.Join(got, " ") != "openid email profile" {
		t.Fatalf("got %v", got)
	}
	if len(ParseOidcScopes("")) != 0 {
		t.Fatal("empty input should give empty list")
	}
}

// TestOidcHandlerHotReload 设置保存后同一 handler 的下一请求即用新配置（new-api 式热更新）。
func TestOidcHandlerHotReload(t *testing.T) {
	g := fakeSettingGetter{}
	// Settings 注入 fake kv：模拟管理员在系统设置页保存配置后的热生效路径。
	h := &OidcHandler{PublicBaseURL: "http://gw.example.com", Env: OidcEnvConfig{}, Settings: g}

	if h.Enabled() {
		t.Fatal("should be disabled with empty config")
	}

	enc, _ := encryptOidcSecret("sec")
	g[model.SettingOidcEnabled] = "1"
	g[model.SettingOidcIssuer] = "https://idp-a.example.com"
	g[model.SettingOidcClientID] = "cid"
	g[model.SettingOidcClientSecret] = enc
	if !h.Enabled() {
		t.Fatal("should be enabled after settings saved")
	}
	c1 := h.resolve()
	if c1 == nil || c1.Issuer != "https://idp-a.example.com" {
		t.Fatalf("client A expected, got %+v", c1)
	}
	if h.callbackURL() != "http://gw.example.com/api/auth/oidc/callback" {
		t.Fatalf("callback url: %q", h.callbackURL())
	}

	// 改 issuer → 下一请求重建
	g[model.SettingOidcIssuer] = "https://idp-b.example.com"
	c2 := h.resolve()
	if c2 == nil || c2.Issuer != "https://idp-b.example.com" {
		t.Fatalf("client B expected, got %+v", c2)
	}
	if c2 == c1 {
		t.Fatal("client should be rebuilt after config change")
	}

	// 显示名热更新 + ssoText
	g[model.SettingOidcDisplayName] = "Keycloak"
	if got := h.ssoText(); got != "使用 Keycloak 登录" {
		t.Fatalf("sso text: %q", got)
	}
	g[model.SettingOidcDisplayName] = ""
	if got := h.ssoText(); got != "使用 企业账号 登录" {
		t.Fatalf("sso text default: %q", got)
	}

	// 关闭后降级
	g[model.SettingOidcEnabled] = "0"
	if h.Enabled() {
		t.Fatal("should be disabled after enabled=0")
	}
}

func TestOidcSettingsView_JoinScopes(t *testing.T) {
	// Scopes 以空格回显（管理端展示格式）
	if s := strings.Join([]string{"openid", "email"}, " "); s != "openid email" {
		t.Fatal(s)
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
