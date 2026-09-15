package conf

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr string
	Database   DatabaseConfig
	JWTSecret  string
	MasterKey  string
	RedisAddr  string
	// RateLimitRPM 每 api_key 每分钟请求数上限；0 = 不限
	RateLimitRPM int
	// RateLimitConcurrency 每 api_key 并发上限；0 = 不限
	RateLimitConcurrency int
	// QuotaEnforce key 级额度硬限额开关（计费）；默认开，关闭后额度只统计不拦截
	QuotaEnforce bool

	// PublicBaseURL 对外可访问的基础地址（OAuth pkce 回调 redirect_uri 拼接用）
	PublicBaseURL string
	// OAuthScanInterval 后台刷新扫描间隔（秒，默认 60）
	OAuthScanInterval int
	// RefreshLead token 过期前多少秒触发刷新（默认 300 = 9router TOKEN_EXPIRY_BUFFER_MS）
	RefreshLead int

	// CORS 允许的 origin 白名单（逗号分隔 env CORS_ALLOWED_ORIGINS；空 = 拒绝跨域）
	CORSAllowedOrigins []string

	// TrustProxy 是否信任反向代理注入的 X-Forwarded-For / X-Real-IP（TRUST_PROXY=1）。
	// 仅在应用端口不可被非可信来源直接访问时开启；否则调用方可伪造 IP，绕过
	// 登录限速与 API Key IP 白名单。
	TrustProxy bool

	// OIDC SSO（Keycloak 兼容任何标准 OIDC IdP）
	OidcEnabled      bool
	OidcIssuer       string
	OidcClientID     string
	OidcClientSecret string
	OidcScopes       []string
}

type DatabaseConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
}

func Load() *Config {
	cfg := &Config{
		ListenAddr: getEnv("LISTEN_ADDR", ":8080"),
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "127.0.0.1"),
			Port:     getEnv("DB_PORT", "5432"),
			User:     getEnv("DB_USER", "keygrid"),
			Password: getEnv("DB_PASSWORD", "keygrid"),
			DBName:   getEnv("DB_NAME", "keygrid"),
		},
		JWTSecret: getEnv("JWT_SECRET", "dev-insecure-secret"),
		MasterKey: getEnv("MASTER_KEY", ""),

		RedisAddr:            getEnv("REDIS_ADDR", "127.0.0.1:6379"),
		RateLimitRPM:         getEnvInt("RATE_LIMIT_RPM", 0),
		RateLimitConcurrency: getEnvInt("RATE_LIMIT_CONCURRENCY", 0),
		QuotaEnforce:         getEnvBool("QUOTA_ENFORCE", true),

		PublicBaseURL:     getEnv("PUBLIC_BASE_URL", "http://127.0.0.1:8080"),
		OAuthScanInterval: getEnvInt("OAUTH_SCAN_INTERVAL", 60),
		RefreshLead:       getEnvInt("OAUTH_REFRESH_LEAD", 300),

		CORSAllowedOrigins: getEnvList("CORS_ALLOWED_ORIGINS"),
		TrustProxy:         getEnvBool("TRUST_PROXY", false),

		OidcEnabled:      getEnvBool("OIDC_ENABLED", false),
		OidcIssuer:       strings.TrimRight(getEnv("OIDC_ISSUER", ""), "/"),
		OidcClientID:     getEnv("OIDC_CLIENT_ID", ""),
		OidcClientSecret: getEnv("OIDC_CLIENT_SECRET", ""),
		OidcScopes:       getEnvList("OIDC_SCOPES"),
	}
	if len(cfg.OidcScopes) == 0 {
		cfg.OidcScopes = []string{"openid", "email", "profile"}
	}
	return cfg
}

// weakDefaults 公开示例/文档中出现过的密钥值：命中即视为未配置（等同公开密钥）。
// 防止“复制 .env.example 后忘改”把示例值带进生产。
var weakDefaults = map[string]string{
	"dev-insecure-secret":                      "JWT_SECRET",
	"dev-insecure-secret-change-me":            "JWT_SECRET",
	"please-change-me-to-a-long-random-secret": "MASTER_KEY",
}

// Validate 拒绝以公开示例默认值启动（值非空但可被任何人推出）。
func (c *Config) Validate() error {
	if name, ok := weakDefaults[c.JWTSecret]; ok {
		return fmt.Errorf("%s is still a public example value; set a strong random secret (e.g. `openssl rand -hex 32`) in .env", name)
	}
	if name, ok := weakDefaults[c.MasterKey]; ok {
		return fmt.Errorf("%s is still a public example value; set a strong random secret (e.g. `openssl rand -hex 32`) in .env", name)
	}
	return nil
}

// getEnvList 逗号分隔 env → []string（空返回 nil）。
func getEnvList(key string) []string {
	v := getEnv(key, "")
	if v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// getEnvBool 接受 1/true/yes/on（大小写不敏感）。
func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	// 未设置（""）= 取 fallback。原实现 `fallback && v != ""` 会把"未设置"错判为 false，
	// 导致带 true 默认值的开关（如 QUOTA_ENFORCE）在未配置时被静默关闭。
	return fallback
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
