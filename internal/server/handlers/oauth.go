package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// oauth.go OAuth 授权流程接口：
//
//	POST /api/providers/{id}/oauth/start  → device_code: {user_code, verification_uri...}；pkce: {authorize_url}
//	GET  /api/providers/{id}/oauth/poll   → 前端轮询；成功后凭据已落库
//	GET  /api/oauth/callback             → pkce 浏览器回调（存 code 到 state，302 到前端）
//
// state 一次性、10min 过期；所有查询 user_id 隔离。
const oauthStateTTL = 10 * time.Minute

// OAuthHandler OAuth 授权流程。
type OAuthHandler struct {
	Op *op.Op
	// PublicBaseURL pkce 回调 redirect_uri（conf.PublicBaseURL）
	PublicBaseURL string
}

// newStateID 一次性 state 标识。
func newStateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// requestAdapter 组装带注入能力的 Callbacks。
// 渠道勾选代理 → token/设备端请求走平台代理（授权流程同样要能到达上游）；
// 勾选代理但平台未配置 → 返回错误，由调用方返回 4xx/5xx。
func (h *OAuthHandler) requestAdapter(r *http.Request, p *model.Provider) (oauth.Callbacks, error) {
	proxyURL := ""
	if p.UseProxy {
		if h.Op == nil {
			return oauth.Callbacks{}, errors.New("channel requires proxy, but platform proxy is not configured")
		}
		v, err := h.Op.ProxyURL()
		if err != nil {
			return oauth.Callbacks{}, err
		}
		if v == "" {
			return oauth.Callbacks{}, errors.New("channel requires proxy, but platform proxy is not configured (admin settings)")
		}
		proxyURL = v
	}
	client, err := httpx.Client(30*time.Second, proxyURL)
	if err != nil {
		return oauth.Callbacks{}, err
	}
	return oauth.Callbacks{
		HTTPClient:    client,
		Hostname:      r.Host,
		PublicBaseURL: h.PublicBaseURL,
	}, nil
}

// Start 发起授权。
func (h *OAuthHandler) Start(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	providerID, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.Op.GetProvider(userID, providerID)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	if p.Kind != "oauth" || p.OAuthProvider == "" {
		resp.BadRequest(w, "provider is not an oauth channel")
		return
	}
	adapter, ok := oauth.Lookup(p.OAuthProvider)
	if !ok {
		resp.BadRequest(w, "unknown oauth provider: "+p.OAuthProvider)
		return
	}

	state := newStateID()
	cb, err := h.requestAdapter(r, p)
	if err != nil {
		resp.BadGateway(w, "oauth client: "+err.Error())
		return
	}
	begin, temp, err := adapter.BeginAuth(r.Context(), cb, state)
	if err != nil {
		log.Printf("[oauth] begin auth failed: %v", err)
		resp.BadGateway(w, "begin auth failed: "+err.Error())
		return
	}
	if err := h.Op.CreateOAuthState(&model.OAuthState{
		State:       state,
		UserID:      userID,
		ProviderKey: p.OAuthProvider,
		Temp:        temp,
		ExpiresAt:   time.Now().Add(oauthStateTTL),
	}); err != nil {
		resp.Internal(w, "save oauth state failed")
		return
	}
	resp.Success(w, begin)
}

// Poll 前端轮询授权结果（device_code 流）。
// pending → 200 {status:"pending"}；成功 → {status:"ok"}（凭据已落库）。
func (h *OAuthHandler) Poll(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	providerID, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.Op.GetProvider(userID, providerID)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}

	// 找该 provider 对应的未过期 state（start 时创建）
	st, err := h.Op.GetLatestOAuthState(userID, p.OAuthProvider)
	if err != nil {
		resp.BadRequest(w, "no pending oauth flow; call /oauth/start first")
		return
	}
	// 凭据已存在 → 上次 poll 已完成
	if cred, err := h.Op.GetCredentialByProviderID(p.ID); err == nil && cred.Status == "active" {
		_ = h.Op.DeleteOAuthState(st.State)
		resp.Success(w, map[string]string{"status": "ok"})
		return
	}

	adapter, ok := oauth.Lookup(st.ProviderKey)
	if !ok {
		resp.BadRequest(w, "unknown oauth provider: "+st.ProviderKey)
		return
	}
	cb, err := h.requestAdapter(r, p)
	if err != nil {
		resp.BadGateway(w, "oauth client: "+err.Error())
		return
	}
	tok, err := adapter.Resolve(r.Context(), cb, st.Temp)
	if err != nil {
		var pending *oauth.PendingError
		if errors.As(err, &pending) {
			resp.Success(w, map[string]string{"status": "pending"})
			return
		}
		log.Printf("[oauth] resolve failed (provider=%s): %v", st.ProviderKey, err)
		// 保留 state，前端可继续轮询/重试；明确失败也返回 pending 语义之外的错误
		resp.BadGateway(w, "oauth resolve failed: "+err.Error())
		return
	}
	if err := h.saveTokenSet(p, tok); err != nil {
		resp.Internal(w, "save credential failed")
		return
	}
	_ = h.Op.DeleteOAuthState(st.State)
	middleware.Audit(h.Op, userID, middleware.AuditEventOAuthDone,
		p.OAuthProvider+" provider="+p.Name, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]string{"status": "ok"})
}

// Callback pkce 浏览器回调：?code=...&state=...（claude 是 code#state 合并传回）。
// 只落 code 到 state.Temp，随即 302 回前端页面；真正换 token 由前端下次 poll 触发。
func (h *OAuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stateID := q.Get("state")
	code := q.Get("code")
	if stateID == "" || code == "" {
		// OAuth error 回调：error=access_denied 等
		resp.BadRequest(w, "missing code/state")
		return
	}
	st, err := h.Op.GetOAuthState(stateID)
	if err != nil {
		resp.BadRequest(w, "invalid or expired state")
		return
	}
	// 归属校验：回调可能被他人携带，但 state 只能由本人 start 生成
	_ = st.UserID

	// claude: code#state 语义由适配器处理，这里透传原始 code
	st.Temp["code"] = code
	if v := q.Get("scope"); v != "" {
		st.Temp["scope"] = v
	}
	if err := h.Op.UpdateOAuthStateTemp(st); err != nil {
		resp.Internal(w, "save callback failed")
		return
	}
	// 302 回前端（简单成功页）
	http.Redirect(w, r, "/oauth/done?state="+stateID, http.StatusFound)
}

// Complete 粘贴回调 URL 完成 pkce 授权（OpenAI Codex：redirect_uri 是用户本机
// localhost:1455，平台域名收不到回调，用户把浏览器最终地址栏的完整 URL 粘贴回来）。
// Body: {"redirect_url": "http://localhost:1455/auth/callback?code=...&state=..."}
// 语义与 Callback 一致：code 落 state.Temp → adapter.Resolve 换 token → 凭据落库。
func (h *OAuthHandler) Complete(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	providerID, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.Op.GetProvider(userID, providerID)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	if p.Kind != "oauth" || p.OAuthProvider == "" {
		resp.BadRequest(w, "provider is not an oauth channel")
		return
	}
	var req struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := resp.Decode(r, &req); err != nil || strings.TrimSpace(req.RedirectURL) == "" {
		resp.BadRequest(w, "redirect_url required")
		return
	}
	u, err := url.Parse(strings.TrimSpace(req.RedirectURL))
	if err != nil {
		resp.BadRequest(w, "invalid redirect_url")
		return
	}
	q := u.Query()
	stateID, code := q.Get("state"), q.Get("code")
	// 上游把错误带回回调（error=access_denied 等）时直接报给用户
	if code == "" && q.Get("error") != "" {
		resp.BadRequest(w, "oauth error: "+q.Get("error")+" "+q.Get("error_description"))
		return
	}
	if stateID == "" || code == "" {
		resp.BadRequest(w, "pasted url must contain code & state query params")
		return
	}
	st, err := h.Op.GetOAuthState(stateID)
	if err != nil {
		resp.BadRequest(w, "invalid or expired state; 请重新发起授权")
		return
	}
	if st.UserID != userID || st.ProviderKey != p.OAuthProvider {
		resp.BadRequest(w, "state does not belong to this provider")
		return
	}

	adapter, ok := oauth.Lookup(st.ProviderKey)
	if !ok {
		resp.BadRequest(w, "unknown oauth provider: "+st.ProviderKey)
		return
	}
	st.Temp["code"] = code
	if v := q.Get("scope"); v != "" {
		st.Temp["scope"] = v
	}
	if err := h.Op.UpdateOAuthStateTemp(st); err != nil {
		resp.Internal(w, "save callback failed")
		return
	}
	cb, err := h.requestAdapter(r, p)
	if err != nil {
		resp.BadGateway(w, "oauth client: "+err.Error())
		return
	}
	tok, err := adapter.Resolve(r.Context(), cb, st.Temp)
	if err != nil {
		log.Printf("[oauth] complete resolve failed (provider=%s): %v", st.ProviderKey, err)
		resp.BadGateway(w, "oauth resolve failed: "+err.Error())
		return
	}
	if err := h.saveTokenSet(p, tok); err != nil {
		resp.Internal(w, "save credential failed")
		return
	}
	_ = h.Op.DeleteOAuthState(st.State)
	middleware.Audit(h.Op, userID, middleware.AuditEventOAuthDone,
		p.OAuthProvider+" provider="+p.Name+" (pasted callback)", middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]string{"status": "ok"})
}

// saveTokenSet TokenSet 加密落库（provider 1:1 credential upsert）。
// 首次授权成功（此前无任何凭据）时把创建时禁用的渠道自动启用；
// 重新授权不动 enabled（保留用户手动停用的意图）。
func (h *OAuthHandler) saveTokenSet(p *model.Provider, tok *oauth.TokenSet) error {
	plain, err := json.Marshal(oauth.TokenSetData{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		Extra:        tok.Extra,
	})
	if err != nil {
		return err
	}
	enc, err := crypto.Encrypt(plain)
	if err != nil {
		return err
	}
	cred := &model.Credential{
		ProviderID: p.ID,
		EncData:    enc,
		Status:     "active",
	}
	if !tok.ExpiresAt.IsZero() {
		cred.ExpiresAt = &tok.ExpiresAt
	}
	firstAuth := false
	if _, err := h.Op.GetCredentialByProviderID(p.ID); err != nil {
		// 仅"无凭据记录"算首次授权；瞬时 DB 错误不误判（否则会把手动停用的渠道意外启用）
		firstAuth = errors.Is(err, gorm.ErrRecordNotFound)
	}
	if err := h.Op.UpsertCredential(cred); err != nil {
		return err
	}
	if firstAuth && !p.Enabled {
		if _, err := h.Op.UpdateProvider(p.UserID, p.ID, func(np *model.Provider) {
			np.Enabled = true
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---- 已有 handler 复用 parseID ----

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return 0, false
	}
	return id, true
}
