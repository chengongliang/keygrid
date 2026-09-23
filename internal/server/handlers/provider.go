package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
	"github.com/chengongliang/keygrid/internal/ssrf"

	"github.com/go-chi/chi/v5"
)

type ProviderHandler struct {
	Op *op.Op
	// Breaker relay 进程内熔断快照（只读引用 + 用户手动重置；nil = 不可用）
	Breaker *relay.Breaker
}

type createProviderReq struct {
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`     // api_key | oauth
	Protocol      string            `json:"protocol"` // openai | anthropic | gemini
	BaseURL       string            `json:"base_url"`
	OAuthProvider string            `json:"oauth_provider"` // kind=oauth: kimi | openai | anthropic
	APIKey        string            `json:"api_key"`        // kind=api_key 必填
	ModelMap      map[string]string `json:"model_map"`
	// BillingMap 计费名映射：{请求名 → 计费标准名}，目标必须是价格表里存在的精确模型名
	BillingMap map[string]string `json:"billing_map"`
	Priority   int               `json:"priority"`
	// UseProxy 上游请求是否走平台代理。nil（前端未传）时 oauth 渠道按预设
	// NeedsProxy 取默认（如 OpenAI 默认 true），api_key 渠道默认 false。
	UseProxy *bool `json:"use_proxy"`
	// BreakerCheck 是否参与熔断检测。缺省 false（不熔断）：只测单一渠道且不稳定时，
	// 熔断后无备选可 failover，不如一直重试。
	BreakerCheck *bool `json:"breaker_check"`
	// UAMode 上游 UA 策略：""（默认透传客户端）/ custom / forward
	UAMode *string `json:"ua_mode"`
	// UserAgent UAMode=custom 时的值
	UserAgent *string `json:"user_agent"`
}

// oauthDefaultBaseURL 各 oauth provider 的默认上游（oauth/presets.go ProviderPresets 单一事实来源）。
// 显式条目只兜底 presets 里没有的 oauth provider（anthropic）；preset 存在时一律以
// preset 为准 —— 历史遗留的 openai = https://chatgpt.com/backend-api 曾因「已占位不覆盖」
// 被钉死，导致新建 Codex 渠道的 base_url 缺 /codex/responses 尾段（请求被 POST 到
// 非 Responses 地址，客户端表现为流中断）。
var oauthDefaultBaseURL = func() map[string]string {
	m := map[string]string{
		"anthropic": "https://api.anthropic.com",
	}
	for _, p := range oauth.ProviderPresets() {
		if p.Kind == "oauth" {
			m[p.Key] = p.BaseURL
		}
	}
	return m
}()

// normalizeOAuthBaseURL oauth 渠道 base_url 落库前归一化（写时归一化）：Codex（openai）
// 渠道约定 base_url 为完整 Responses endpoint，历史默认值与手工填写常漏尾段，
// 统一补全。relay 转发侧同样兜底（relay.NormalizeCodexBaseURL），存量渠道无需迁移。
func normalizeOAuthBaseURL(kind, oauthProvider, baseURL string) string {
	if kind == "oauth" && oauthProvider == "openai" {
		return relay.NormalizeCodexBaseURL(baseURL)
	}
	if kind == "oauth" && oauthProvider == "xai" {
		return relay.NormalizeXAIBaseURL(baseURL)
	}
	return baseURL
}

// sortedOAuthKeys 已注册 oauth provider keys（排序稳定，错误提示用）。
func sortedOAuthKeys() []string {
	ks := oauth.Keys()
	sort.Strings(ks)
	return ks
}

// Meta GET /api/providers/meta —— 渠道类型元信息（前端向导第一步渲染用）。
// proxy_url：平台出口代理（密码脱敏后）—— 前端在「走代理访问」勾选框上展示
// 具体地址；空串 = 未配置代理，前端不展示勾选框。
func (h *ProviderHandler) Meta(w http.ResponseWriter, _ *http.Request) {
	proxyURL, _ := h.Op.ProxyURL()
	masked := ""
	if proxyURL != "" {
		masked = httpx.MaskProxyURL(proxyURL)
	}
	resp.Success(w, map[string]any{
		"protocols":   []string{"openai", "anthropic", "gemini"},
		"oauth_keys":  sortedOAuthKeys(),
		"oauth_flows": oauthFlowMeta(),
		"presets":     presetsForUI(),
		"proxy_url":   masked,
	})
}

// oauthFlowMeta 各 oauth provider 的流程类型（前端区分 device_code 展示 user_code /
// auth_code·pkce 跳转浏览器）。
func oauthFlowMeta() map[string]string {
	m := map[string]string{}
	for _, k := range oauth.Keys() {
		if a, ok := oauth.Lookup(k); ok {
			m[k] = string(a.Flow())
		}
	}
	return m
}

// presetsForUI 预设列表 + 动态模型目录注入：有远程刷新目录的 oauth provider
// （openai/Codex）用动态目录覆盖硬编码 Models（目录拉取失败时 CodexModelCatalog
// 自带内嵌快照兑底，硬编码 Models 是最后防线）。前端向导预填与「拉取模型」保持一致。
func presetsForUI() []oauth.ProviderPreset {
	out := oauth.ProviderPresets()
	for i := range out {
		if models := oauth.CodexModelCatalog(out[i].Key); len(models) > 0 {
			out[i].Models = models
		}
	}
	return out
}

// Presets GET /api/providers/presets —— 预设模板全量列表（base_url/模型目录/认证方式）。
func (h *ProviderHandler) Presets(w http.ResponseWriter, _ *http.Request) {
	resp.Success(w, presetsForUI())
}

// isE2EHost compose 网络内的 mock 服务域名（SSRF 校验放行，仅测试环境）。
// 生产 SSRF 防护不受影响 —— 私网 IP 依然全部拒绝。
func isE2EHost(rawURL string) bool {
	if os.Getenv("SSRF_ALLOW_INTERNAL") != "1" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return strings.HasSuffix(h, ".internal") || h == "mock-kimi" || h == "host.docker.internal"
}

func (h *ProviderHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createProviderReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	if req.Name == "" || req.Protocol == "" {
		resp.BadRequest(w, "name and protocol are required")
		return
	}
	if req.Kind == "" {
		req.Kind = "api_key"
	}
	userID := middleware.UserID(r.Context())

	switch req.Kind {
	case "api_key":
		if req.BaseURL == "" {
			resp.BadRequest(w, "base_url required for kind=api_key")
			return
		}
		if req.APIKey == "" {
			resp.BadRequest(w, "api_key required for kind=api_key")
			return
		}
	case "oauth":
		if !oauth.IsRegistered(req.OAuthProvider) {
			resp.BadRequest(w, "unknown oauth_provider ("+strings.Join(sortedOAuthKeys(), "|")+")")
			return
		}
		// oauth 渠道 base_url 有默认值（授权完成后才可转发）
		if req.BaseURL == "" {
			req.BaseURL = oauthDefaultBaseURL[req.OAuthProvider]
		}
		req.BaseURL = normalizeOAuthBaseURL(req.Kind, req.OAuthProvider, req.BaseURL)
	default:
		resp.BadRequest(w, "kind must be api_key or oauth")
		return
	}
	if req.BaseURL == "" {
		resp.BadRequest(w, "base_url required")
		return
	}
	// SSRF 防护 —— base_url 拒绝私网/环回（e2e 场景 compose 内网域名先放行）
	if !isE2EHost(req.BaseURL) {
		if err := ssrf.CheckBaseURL(req.BaseURL); err != nil {
			resp.BadRequest(w, err.Error())
			return
		}
	}

	// 渠道级出口代理：未显式勾选时按预设默认（OpenAI 等需代理站点默认启用）。
	// 平台未配置代理时勾选无意义：一律降级为直连（与前端「未配置不展示勾选框」
	// 一致，避免员工在无代理部署里加 OpenAI 渠道被拒；配置了代理的正常路径不受影响）。
	useProxy := false
	if req.UseProxy != nil {
		useProxy = *req.UseProxy
	} else if req.Kind == "oauth" {
		if preset, ok := oauth.LookupPreset(req.OAuthProvider); ok {
			useProxy = preset.NeedsProxy
		}
	}
	if useProxy && !h.proxyAvailable() {
		useProxy = false
	}

	// UA 策略校验：mode ∈ {默认/custom/forward}；custom 必须带合法值
	// （禁 CR/LF 防 header 注入，≤256）
	uaMode := ""
	if req.UAMode != nil {
		uaMode = *req.UAMode
	}
	if !relay.ValidUAMode(uaMode) {
		resp.BadRequest(w, "invalid ua_mode")
		return
	}
	uaValue := ""
	if req.UserAgent != nil {
		uaValue = strings.TrimSpace(*req.UserAgent)
	}
	if uaMode == relay.UAModeCustom && !relay.ValidUserAgent(uaValue) {
		resp.BadRequest(w, "invalid user_agent: 自定义 UA 不能为空、不能含换行，且不超过 256 字符")
		return
	}

	// 计费名映射校验：目标必须在价格表内（映射只做名称归一化，改不了价格）
	if err := h.validateBillingMap(req.BillingMap); err != nil {
		resp.BadRequest(w, err.Error())
		return
	}
	p := &model.Provider{
		UserID:        userID,
		Name:          req.Name,
		Kind:          req.Kind,
		Protocol:      req.Protocol,
		BaseURL:       req.BaseURL,
		OAuthProvider: req.OAuthProvider,
		ModelMap:      req.ModelMap,
		BillingMap:    req.BillingMap,
		Priority:      req.Priority,
		UseProxy:      useProxy,
		BreakerCheck:  req.BreakerCheck != nil && *req.BreakerCheck,
		UAMode:        uaMode,
		UserAgent:     uaValue,
		// oauth 渠道授权完成前不参与转发：创建时禁用，saveTokenSet 首次授权成功后自动启用
		Enabled: req.Kind == "api_key",
	}
	if err := h.Op.CreateProvider(p); err != nil {
		resp.Internal(w, "save provider failed")
		return
	}
	// api_key 凭据立即落库；oauth 凭据等授权流程完成后由 oauth handler 写入
	if req.Kind == "api_key" {
		if err := h.saveCredential(p.ID, req.APIKey); err != nil {
			resp.Internal(w, "save credential failed")
			return
		}
		p.Authorized = true
	}
	middleware.Audit(h.Op, userID, middleware.AuditEventProviderCreate,
		req.Kind+"/"+req.OAuthProvider+" "+req.Name+" base_url="+req.BaseURL, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, p)
}

// saveCredential AES-GCM 加密后落库（明文不落任何地方）。
func (h *ProviderHandler) saveCredential(providerID int64, apiKey string) error {
	plain, _ := json.Marshal(map[string]string{"api_key": apiKey})
	enc, err := crypto.Encrypt(plain)
	if err != nil {
		return err
	}
	return h.Op.UpsertCredential(&model.Credential{
		ProviderID: providerID,
		EncData:    enc,
		Status:     "active",
	})
}

func (h *ProviderHandler) List(w http.ResponseWriter, r *http.Request) {
	ps, err := h.Op.ListProviders(middleware.UserID(r.Context()))
	if err != nil {
		resp.Internal(w, "list failed")
		return
	}
	// 填充授权标记：api_key 恒 true；oauth = 存在 active 凭据（前端"待授权"徽标 + 隐藏启停按钮）
	statuses, err := h.Op.CredentialStatusByProviderIDs(providerIDs(ps))
	if err != nil {
		resp.Internal(w, "list failed")
		return
	}
	for i := range ps {
		if ps[i].Kind == "oauth" {
			ps[i].Authorized = statuses[ps[i].ID] == "active"
		} else {
			ps[i].Authorized = true
		}
	}
	resp.Success(w, ps)
}

// providerIDs 渠道 id 列表（oauth 渠道批量查凭据状态用）。
func providerIDs(ps []model.Provider) []int64 {
	ids := make([]int64, 0, len(ps))
	for i := range ps {
		if ps[i].Kind == "oauth" {
			ids = append(ids, ps[i].ID)
		}
	}
	return ids
}

func (h *ProviderHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	p, err := h.Op.GetProvider(middleware.UserID(r.Context()), id)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	fillAuthorized(h.Op, p)
	resp.Success(w, p)
}

// fillAuthorized 单渠道授权标记回填（Get/Update 响应用，语义与 List 一致）：
// api_key 恒 true；oauth = 存在 active 凭据。
func fillAuthorized(o *op.Op, p *model.Provider) {
	if p.Kind != "oauth" {
		p.Authorized = true
		return
	}
	if cred, err := o.GetCredentialByProviderID(p.ID); err == nil {
		p.Authorized = cred.Status == "active"
	}
}

type updateProviderReq struct {
	Name       *string           `json:"name"`
	BaseURL    *string           `json:"base_url"`
	Protocol   *string           `json:"protocol"` // openai | anthropic | gemini
	ModelMap   map[string]string `json:"model_map"`
	BillingMap map[string]string `json:"billing_map"` // 计费名映射；非 nil 即整体替换
	Priority   *int              `json:"priority"`
	Enabled    *bool             `json:"enabled"`
	UseProxy   *bool             `json:"use_proxy"`
	// BreakerCheck 是否参与熔断检测（默认不熔断）
	BreakerCheck *bool   `json:"breaker_check"`
	UAMode       *string `json:"ua_mode"`
	UserAgent    *string `json:"user_agent"`
	APIKey       *string `json:"api_key"` // 传入则轮换凭据
}

// proxyAvailable 平台是否已配置出口代理（TTL 缓存内的新配置也一致）。
func (h *ProviderHandler) proxyAvailable() bool {
	v, err := h.Op.ProxyURL()
	return err == nil && v != ""
}

// validateBillingMap 计费名映射校验（安全约束，不可妥协）：
// 映射目标必须是全局价格表里存在的精确模型名（"*" 兜底行不可作为映射目标）——
// 映射只做名称归一化，改不了价格，定价权始终在 admin。
func (h *ProviderHandler) validateBillingMap(m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	ps, err := h.Op.ListModelPrices()
	if err != nil {
		return errors.New("查询价格表失败，请稍后再试")
	}
	priced := make(map[string]bool, len(ps))
	for _, p := range ps {
		if p.Model != "*" {
			priced[p.Model] = true
		}
	}
	for from, to := range m {
		if strings.TrimSpace(from) == "" {
			return errors.New("billing_map 存在空白的请求模型名")
		}
		if strings.TrimSpace(to) == "" {
			return fmt.Errorf("billing_map[%s] 映射目标为空", from)
		}
		if to == "*" {
			return fmt.Errorf("billing_map[%s] 不能映射到兜底价 *", from)
		}
		if !priced[to] {
			return fmt.Errorf("billing_map[%s] 映射目标 %s 不在价格表中，请先联系管理员添加该模型定价", from, to)
		}
	}
	return nil
}

func (h *ProviderHandler) Update(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	var req updateProviderReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	// SSRF：与 Create 同防 —— base_url 拒绝私网/环回（防借网关转发探测内网）
	if req.BaseURL != nil && *req.BaseURL != "" && !isE2EHost(*req.BaseURL) {
		if err := ssrf.CheckBaseURL(*req.BaseURL); err != nil {
			resp.BadRequest(w, err.Error())
			return
		}
	}
	// 渠道级出口代理：平台未配置时降级为直连（语义同 Create，不报错）
	useProxy := false
	if req.UseProxy != nil {
		useProxy = *req.UseProxy
		if useProxy && !h.proxyAvailable() {
			useProxy = false
		}
	}
	// 兜底拦截：未授权 oauth 渠道不允许启用（无凭据时启用既无法转发，
	// 又会把 model_map 暴露进网关 /v1/models；授权完成由 oauth 流程自动启用）
	if req.Enabled != nil && *req.Enabled {
		cur, err := h.Op.GetProvider(userID, id)
		if err != nil {
			resp.NotFound(w, "provider not found")
			return
		}
		if cur.Kind == "oauth" {
			if cred, err := h.Op.GetCredentialByProviderID(id); err != nil || cred.Status != "active" {
				resp.BadRequest(w, "oauth channel is not authorized yet; complete authorization first")
				return
			}
		}
	}
	if req.BillingMap != nil {
		if err := h.validateBillingMap(req.BillingMap); err != nil {
			resp.BadRequest(w, err.Error())
			return
		}
	}
	// UA 策略校验：必须在写库前完成（UpdateProvider 闭包里的 return 无法阻止
	// 已经发生的部分字段更新）。custom 时必须带合法值（禁 CR/LF 防 header 注入）。
	if req.UAMode != nil || req.UserAgent != nil {
		cur, err := h.Op.GetProvider(userID, id)
		if err != nil {
			resp.NotFound(w, "provider not found")
			return
		}
		mode := cur.UAMode
		if req.UAMode != nil {
			mode = *req.UAMode
		}
		if !relay.ValidUAMode(mode) {
			resp.BadRequest(w, "invalid ua_mode")
			return
		}
		value := cur.UserAgent
		if req.UserAgent != nil {
			value = strings.TrimSpace(*req.UserAgent)
		}
		if mode == relay.UAModeCustom && !relay.ValidUserAgent(value) {
			resp.BadRequest(w, "invalid user_agent: 自定义 UA 不能为空、不能含换行，且不超过 256 字符")
			return
		}
	}
	p, err := h.Op.UpdateProvider(userID, id, func(p *model.Provider) {
		if req.Name != nil {
			p.Name = *req.Name
		}
		if req.BaseURL != nil {
			b := *req.BaseURL
			if b == "" && p.Kind == "oauth" {
				// oauth 渠道编辑弹窗放开 base_url 后允许「清空恢复默认」，
				// 不能把上游地址直接置空（渠道会彻底不可用）
				b = oauthDefaultBaseURL[p.OAuthProvider]
			}
			p.BaseURL = normalizeOAuthBaseURL(p.Kind, p.OAuthProvider, b)
		}
		if req.Protocol != nil {
			p.Protocol = *req.Protocol
		}
		if req.ModelMap != nil {
			p.ModelMap = req.ModelMap
		}
		if req.BillingMap != nil {
			p.BillingMap = req.BillingMap
		}
		if req.Priority != nil {
			p.Priority = *req.Priority
		}
		if req.Enabled != nil {
			p.Enabled = *req.Enabled
		}
		if req.UseProxy != nil {
			p.UseProxy = useProxy
		}
		if req.BreakerCheck != nil {
			p.BreakerCheck = *req.BreakerCheck
		}
		if req.UAMode != nil {
			p.UAMode = *req.UAMode
		}
		if req.UserAgent != nil {
			p.UserAgent = strings.TrimSpace(*req.UserAgent)
		}
	})
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	if req.APIKey != nil && *req.APIKey != "" {
		if err := h.saveCredential(id, *req.APIKey); err != nil {
			resp.Internal(w, "save credential failed")
			return
		}
	}
	changes := make([]string, 0, 6)
	if req.Name != nil {
		changes = append(changes, "name")
	}
	if req.BaseURL != nil {
		changes = append(changes, "base_url")
	}
	if req.Protocol != nil {
		changes = append(changes, "protocol")
	}
	if req.ModelMap != nil {
		changes = append(changes, "model_map")
	}
	if req.BillingMap != nil {
		changes = append(changes, "billing_map")
	}
	if req.Priority != nil {
		changes = append(changes, "priority")
	}
	if req.Enabled != nil {
		changes = append(changes, "enabled")
	}
	if req.UseProxy != nil {
		changes = append(changes, "use_proxy")
	}
	if req.BreakerCheck != nil {
		changes = append(changes, "breaker_check")
	}
	if req.UAMode != nil || req.UserAgent != nil {
		changes = append(changes, "user_agent")
	}
	if req.APIKey != nil {
		changes = append(changes, "credential(rotated)")
	}
	middleware.Audit(h.Op, userID, middleware.AuditEventProviderUpdate,
		p.Name+" fields="+strings.Join(changes, ","), middleware.ClientIP(r), r.UserAgent())
	fillAuthorized(h.Op, p)
	resp.Success(w, p)
}

func (h *ProviderHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	if err := h.Op.DeleteProvider(userID, id); err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	middleware.Audit(h.Op, userID, middleware.AuditEventProviderDelete,
		strconv.FormatInt(id, 10), middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"deleted": true})
}
