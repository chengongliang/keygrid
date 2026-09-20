package router

import (
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/conf"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/quota"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/handlers"
	mw "github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
	"github.com/chengongliang/keygrid/internal/ssrf"
	"github.com/chengongliang/keygrid/web"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
)

func New(o *op.Op, jwtSecret string) http.Handler {
	return NewWithConfig(o, &conf.Config{JWTSecret: jwtSecret})
}

// NewWithConfig 组装路由：relay 引擎带限流/记账、OAuth、Web+安全。
// webFS 为 nil 时使用 embed 的前端静态资源。
func NewWithConfig(o *op.Op, cfg *conf.Config) http.Handler {
	return NewWithOptions(o, cfg, nil)
}

// NewWithOptions router 组装入口（webFS 支持 dev 注入 os.DirFS）。
func NewWithOptions(o *op.Op, cfg *conf.Config, webFS fs.FS) http.Handler {
	r := chi.NewRouter()

	loginLimiter := mw.NewLoginRateLimiter()
	auditH := &handlers.AuditHandler{Op: o}
	testH := &handlers.TestHandler{Op: o}

	// RealIP 仅在可信反向代理后启用（TRUST_PROXY=1）：直连时信任 X-Forwarded-For
	// 会让调用方伪造客户端 IP，绕过登录限速与 API Key 白名单。
	mid := []func(http.Handler) http.Handler{chimw.Logger, chimw.Recoverer, mw.CORS(cfg.CORSAllowedOrigins)}
	if cfg.TrustProxy {
		mid = append([]func(http.Handler) http.Handler{chimw.RealIP}, mid...)
	}
	r.Use(mid...)

	authH := &handlers.AuthHandler{Op: o, JWTSecret: cfg.JWTSecret}
	keyH := &handlers.ApiKeyHandler{Op: o}
	provH := &handlers.ProviderHandler{Op: o}
	usageH := &handlers.UsageHandler{Op: o}
	oauthH := &handlers.OAuthHandler{Op: o, PublicBaseURL: cfg.PublicBaseURL}
	pricesH := &handlers.PricesHandler{Op: o}
	relayH := relay.NewHandler(o)
	// 用户端渠道健康：注入熔断快照引用（只读 + 用户手动重置自己渠道）
	provH.Breaker = relayH.Breaker

	// OIDC SSO —— 配置存平台设置（管理员系统设置页动态修改、保存即热生效，参考 new-api），
	// OIDC_* 环境变量作兜底；未配置时端点自动降级（Enabled()=false）。
	oidcH := &handlers.OidcHandler{
		Op:            o,
		PublicBaseURL: cfg.PublicBaseURL,
		JWTSecret:     cfg.JWTSecret,
		Env: handlers.OidcEnvConfig{
			Enabled:      cfg.OidcEnabled,
			Issuer:       cfg.OidcIssuer,
			ClientID:     cfg.OidcClientID,
			ClientSecret: cfg.OidcClientSecret,
			Scopes:       cfg.OidcScopes,
		},
	}

	// usage 异步批量记账
	uw := relay.NewUsageWriter(o, 100, 2*time.Second)
	relayH.SetUsageWriter(uw)
	// 失败诊断（request_errors）异步批量写入：渠道故障期不阻塞转发
	relayH.SetErrorWriter(relay.NewErrorWriter(o, 50, 2*time.Second))

	// 计费： 模型价格缓存（30s TTL 内存缓存，转发热路径零 DB 查询）
	relayH.SetPricing(relay.NewPricing(o))

	// Redis 限流（Redis 连不上则自动 fail-open，不限流）
	var rdb *redis.Client
	if cfg.RedisAddr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
		relayH.SetRateLimiter(relay.NewRateLimiter(rdb, cfg.RateLimitRPM, cfg.RateLimitConcurrency))
	}

	// 计费： key 级额度硬限额（QUOTA_ENFORCE 默认开；Redis 不可用时降级 DB 检查，不 fail-open）
	quotaEnforcer := relay.NewQuotaEnforcer(rdb, o, cfg.QuotaEnforce)
	relayH.SetQuotaEnforcer(quotaEnforcer)
	keyH.Quota = quotaEnforcer

	// OAuth 调度器（relay 请求时兜底刷新；main.go 另起后台扫描）
	oauthSched := oauth.NewScheduler(o, rdb, time.Duration(cfg.RefreshLead)*time.Second)
	relayH.SetOAuthScheduler(oauthSched, time.Duration(cfg.RefreshLead)*time.Second)

	// 额度同步：relay 被动观察转发响应的 x-codex-* 头（主动探测在 task.Runner/main）
	quotaSyncer := quota.NewSyncer(o)
	relayH.SetQuotaSyncer(quotaSyncer)
	quotaH := &handlers.QuotaHandler{Op: o, Syncer: quota.NewSyncer(o)}

	// SSRF 防护 transport（上游转发强制走私网拒绝 Dialer）
	relayH.HTTPClient = &http.Client{Timeout: 10 * time.Minute, Transport: ssrf.SafeTransport(10 * time.Minute)}

	// 健康检查
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		resp.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// 管理面 API
	r.Route("/api", func(api chi.Router) {
		// 登录/注册：IP 限速
		api.Group(func(pub chi.Router) {
			pub.Use(mw.RateLimitByIP(loginLimiter))
			pub.Post("/auth/register", authH.Register)
			pub.Post("/auth/login", authH.Login)
			// SSO 发起也限速：state 每次 INSERT，放弃登录（IdP 处关页）很常见
			pub.Get("/auth/oidc/login", oidcH.Login)
		})

		api.Group(func(authed chi.Router) {
			authed.Use(mw.SessionAuth(cfg.JWTSecret))
			authed.Get("/auth/me", authH.Me)
			authed.Get("/keys", keyH.List)
			authed.Post("/keys", keyH.Create)
			authed.Patch("/keys/{id}", keyH.Update)
			authed.Get("/keys/{id}/reveal", keyH.Reveal)
			authed.Delete("/keys/{id}", keyH.Delete)

			// 渠道可用性（健康快照 / 失败详情 / 熔断重置）：均按 user_id 收窄
			authed.Get("/providers/health", provH.Health)
			authed.Get("/providers", provH.List)
			authed.Get("/providers/{id}", provH.Get)
			authed.Post("/providers", provH.Create)
			authed.Patch("/providers/{id}", provH.Update)
			authed.Delete("/providers/{id}", provH.Delete)
			authed.Get("/providers/{id}/errors", provH.Errors)
			authed.Post("/providers/{id}/breaker/reset", provH.ResetBreaker)
			authed.Post("/providers/{id}/test", testH.TestProvider)
			authed.Post("/providers/{id}/models", testH.FetchUpstreamModels)
			authed.Post("/providers/{id}/test_model", testH.TestModel)
			authed.Post("/providers/{id}/test_model_stream", testH.TestModelStream)
			authed.Post("/providers/probe_models", testH.ProbeModels)

			// 渠道预设模板（前端添加渠道向导）
			authed.Get("/providers/meta", provH.Meta)
			authed.Get("/providers/presets", provH.Presets)

			// OAuth 授权流程
			authed.Post("/providers/{id}/oauth/start", oauthH.Start)
			authed.Get("/providers/{id}/oauth/poll", oauthH.Poll)
			// Codex：回调落在用户本机 localhost:1455，平台收不到 —— 粘贴回调 URL 完成授权
			authed.Post("/providers/{id}/oauth/complete", oauthH.Complete)

			// 额度查询（openai codex /wham/usage 同步结果）：List 读快照、Refresh 手动探测
			authed.Get("/quota", quotaH.List)
			authed.Post("/providers/{id}/quota/refresh", quotaH.Refresh)

			authed.Get("/usage", usageH.Get)
			authed.Get("/usage/hourly", usageH.UsageHourly)
			authed.Get("/usage/by-key", usageH.ByKey)
			authed.Get("/usage/by-model", usageH.ByModel)
			authed.Get("/usage/by-provider", usageH.ByProvider)
			authed.Get("/usage/summary", usageH.Summary)
			authed.Get("/audit", auditH.List)

			// 计费： 价格表用户侧只读（映射下拉 / 费用透明；写入只走 admin 端）
			authed.Get("/prices", pricesH.List)
		})

		// 管理面（RequireRole("admin")）
		adminAuth := mw.RequireRole("admin")
		usersH := &handlers.AdminUsersHandler{Op: o}
		adminUsageH := &handlers.AdminUsageHandler{Op: o}
		adminProvH := &handlers.AdminProvidersHandler{Op: o, Breaker: relayH.Breaker}
		adminPricesH := &handlers.AdminPricesHandler{Op: o}
		settingsH := &handlers.AdminSettingsHandler{Op: o, OidcEnv: oidcH.Env, PublicBaseURL: cfg.PublicBaseURL}
		api.Route("/admin", func(admin chi.Router) {
			admin.Use(mw.SessionAuth(cfg.JWTSecret), adminAuth)
			admin.Get("/users", usersH.List)
			admin.Patch("/users/{id}", usersH.Update)
			admin.Post("/users/{id}/reset_password", usersH.ResetPassword)
			admin.Get("/usage", adminUsageH.Get)
			admin.Get("/usage/top", adminUsageH.Top)
			admin.Get("/usage/hourly", adminUsageH.Hourly)
			admin.Get("/usage/export", adminUsageH.Export)
			admin.Get("/providers", adminProvH.List)
			admin.Post("/providers/{id}/breaker/reset", adminProvH.ResetBreaker)
			admin.Get("/providers/{id}/errors", adminProvH.Errors)
			// 计费： 价格表管理
			admin.Get("/prices", adminPricesH.List)
			admin.Post("/prices", adminPricesH.Upsert)
			admin.Delete("/prices/{id}", adminPricesH.Delete)
			admin.Get("/prices/unpriced", adminPricesH.Unpriced)
			// 定价一键同步（OpenRouter）：preview 只读对比，commit 批量写入勾选项
			admin.Get("/prices/sync/preview", adminPricesH.SyncPreview)
			admin.Post("/prices/sync", adminPricesH.SyncCommit)
			admin.Get("/settings", settingsH.Get)
			admin.Put("/settings", settingsH.Put)
			admin.Get("/invitations", settingsH.ListInvitations)
			admin.Post("/invitations", settingsH.CreateInvitation)
		})

		// SSO 配置 + 注册策略（登录页渲染 SSO 按钮/注册入口用，公开）
		api.Get("/auth/config", oidcConfigHandler(oidcH, o))

		// OIDC SSO 端点（oidcH 为 nil 时统一降级 404；login 已在上方限速组内）
		api.Get("/auth/oidc/callback", oidcH.Callback)
		api.Get("/auth/logout", oidcH.Logout)
		api.Post("/auth/logout", oidcH.Logout)
		api.Get("/config", func(w http.ResponseWriter, _ *http.Request) {
			ann, _ := o.GetSetting("announcement")
			mm, _ := o.GetMaintenanceMode()
			rp, err := o.GetSetting("registration_policy")
			if err != nil || rp == "" {
				rp = "open"
			}
			resp.Success(w, map[string]any{
				"announcement":        ann,
				"maintenance_mode":    mm,
				"registration_policy": rp,
			})
		})
	})

	// OAuth pkce 浏览器回调（无需鉴权：state 本身是能力凭证）
	r.Get("/api/oauth/callback", oauthH.Callback)
	// 前端 pkce 回调落地页（302 目标）
	r.Get("/oauth/done", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>OAuth</title></head>
<body style="background:#09090b;color:#e4e4e7;font-family:system-ui;display:flex;align-items:center;justify-content:center;height:100vh">
<div style="text-align:center"><h2>✅ 授权回调已收到</h2><p style="color:#a1a1aa">请回到网关页面，点击「我已授权，继续」完成配置。</p></div>
</body></html>`))
	})

	// 转发面（多协议：openai chat/completions + anthropic messages + responses，sk- key 鉴权）
	r.Route("/v1", func(v1 chi.Router) {
		// 维护模式 gate 必须在 ApiKeyAuth 之前 —— 维护中无效 key 也要 503 而非 401
		v1.Use(mw.MaintenanceGate(o), mw.ApiKeyAuth(o))
		v1.Post("/chat/completions", relayH.ChatCompletions)
		v1.Post("/messages", relayH.Messages)
		v1.Post("/responses", relayH.Responses)
		v1.Get("/models", relayH.Models)
	})

	// 前端 SPA（embed 静态资源）
	if webFS == nil {
		webFS = web.Dist()
	}
	r.Get("/*", spaHandler(webFS))

	return r
}

// oidcConfigHandler SSO 配置端点（公开，含注册策略）。oidcH 为 nil 时返回 disabled（防御分支，实际构造恒非 nil）。
func oidcConfigHandler(h *handlers.OidcHandler, o *op.Op) http.HandlerFunc {
	if h == nil {
		return func(w http.ResponseWriter, _ *http.Request) {
			rp, err := o.GetSetting("registration_policy")
			if err != nil || rp == "" {
				rp = "open"
			}
			resp.Success(w, map[string]any{"oidc_enabled": false, "sso_text": "", "registration_policy": rp})
		}
	}
	return h.Config
}

// spaHandler SPA 静态资源 + history 路由回退（非 /api /v1 路径 → index.html）。
func spaHandler(fsys fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(fsys))
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(fsys, path); err != nil {
			// 路由不存在 → 回退 index.html（history 模式）
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	}
}
