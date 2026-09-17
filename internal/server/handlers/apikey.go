package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/go-chi/chi/v5"
)

type ApiKeyHandler struct {
	Op *op.Op
	// Quota 额度检查器（计费；重置用量时删 Redis 实时键；nil 时跳过）
	Quota *relay.QuotaEnforcer
}

type createKeyReq struct {
	Name          string  `json:"name"`
	ExpiresAt     string  `json:"expires_at"`     // RFC3339, 可空
	IPWhitelist   string  `json:"ip_whitelist"`   // 逗号分隔 IP/CIDR, 空 = 不限
	ModelLimit    string  `json:"model_limit"`    // 逗号分隔模型名, 空 = 不限
	ProviderLimit string  `json:"provider_limit"` // 逗号分隔渠道 ID, 空 = 不限
	QuotaLimit    float64 `json:"quota_limit"`    // 额度上限 USD；0 = 不限（计费硬拦截）
}

// Create 签发新 key：明文仅此一次返回，库存 sha256 + AES-GCM 加密明文（供后续查看/复制）。
func (h *ApiKeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createKeyReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	if !validIPWhitelist(req.IPWhitelist) {
		resp.BadRequest(w, "ip_whitelist 格式无效：请填写逗号分隔的 IP 或 CIDR，如 1.2.3.4,10.0.0.0/8")
		return
	}
	userID := middleware.UserID(r.Context())

	providerLimit, err := h.normalizeProviderLimit(userID, req.ProviderLimit)
	if err != nil {
		resp.BadRequest(w, err.Error())
		return
	}

	// 生成 256bit 随机 key
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		resp.Internal(w, "generate key failed")
		return
	}
	plaintext := "sk-" + hex.EncodeToString(buf)
	enc, err := crypto.Encrypt([]byte(plaintext))
	if err != nil {
		resp.Internal(w, "encrypt key failed")
		return
	}

	sum := sha256.Sum256([]byte(plaintext))
	if req.QuotaLimit < 0 {
		resp.BadRequest(w, "quota_limit 不能为负数")
		return
	}
	key := &model.ApiKey{
		UserID:        userID,
		Name:          req.Name,
		KeyHash:       hex.EncodeToString(sum[:]),
		KeyEnc:        enc,
		Prefix:        plaintext[:10],
		IPWhitelist:   strings.TrimSpace(req.IPWhitelist),
		ModelLimit:    normalizeModelLimit(req.ModelLimit),
		ProviderLimit: providerLimit,
		QuotaLimit:    req.QuotaLimit,
		Enabled:       true,
	}
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			resp.BadRequest(w, "expires_at must be RFC3339")
			return
		}
		key.ExpiresAt = &t
	}
	if err := h.Op.CreateApiKey(key); err != nil {
		resp.Internal(w, "save key failed")
		return
	}
	middleware.Audit(h.Op, userID, middleware.AuditEventKeyCreate,
		"name="+req.Name+" prefix="+key.Prefix, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{
		"id":      key.ID,
		"name":    key.Name,
		"prefix":  key.Prefix,
		"api_key": plaintext, // ⚠️ 明文仅展示一次
	})
}

func (h *ApiKeyHandler) List(w http.ResponseWriter, r *http.Request) {
	keys, err := h.Op.ListApiKeys(middleware.UserID(r.Context()))
	if err != nil {
		resp.Internal(w, "list failed")
		return
	}
	resp.Success(w, keys)
}

// updateKeyReq 部分更新：nil = 不改。
type updateKeyReq struct {
	Name          *string  `json:"name"`
	Enabled       *bool    `json:"enabled"`
	ExpiresAt     *string  `json:"expires_at"` // RFC3339；"" = 清除过期
	IPWhitelist   *string  `json:"ip_whitelist"`
	ModelLimit    *string  `json:"model_limit"`
	ProviderLimit *string  `json:"provider_limit"` // 逗号分隔渠道 ID；"" = 取消绑定
	QuotaLimit    *float64 `json:"quota_limit"`    // 额度上限 USD；改小不追讨已消耗
	ResetQuota    *bool    `json:"reset_quota"`    // true = 重置已用额度（清零 quota_used + Redis 键）
}

// Update 编辑 key（名称/启禁用/过期/IP限制/模型限制）。
func (h *ApiKeyHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	var req updateKeyReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	if req.IPWhitelist != nil && !validIPWhitelist(*req.IPWhitelist) {
		resp.BadRequest(w, "ip_whitelist 格式无效：请填写逗号分隔的 IP 或 CIDR，如 1.2.3.4,10.0.0.0/8")
		return
	}
	userID := middleware.UserID(r.Context())
	var invalidQuota bool
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			resp.BadRequest(w, "expires_at must be RFC3339")
			return
		}
		expiresAt = &t
	}
	var providerLimit *string
	if req.ProviderLimit != nil {
		norm, err := h.normalizeProviderLimit(userID, *req.ProviderLimit)
		if err != nil {
			resp.BadRequest(w, err.Error())
			return
		}
		providerLimit = &norm
	}
	key, err := h.Op.UpdateApiKey(userID, id, func(k *model.ApiKey) {
		if req.Name != nil {
			k.Name = *req.Name
		}
		if req.Enabled != nil {
			k.Enabled = *req.Enabled
		}
		if req.ExpiresAt != nil {
			k.ExpiresAt = expiresAt
		}
		if req.IPWhitelist != nil {
			k.IPWhitelist = strings.TrimSpace(*req.IPWhitelist)
		}
		if req.ModelLimit != nil {
			k.ModelLimit = normalizeModelLimit(*req.ModelLimit)
		}
		if providerLimit != nil {
			k.ProviderLimit = *providerLimit
		}
		if req.QuotaLimit != nil {
			if *req.QuotaLimit < 0 {
				invalidQuota = true
				return
			}
			k.QuotaLimit = *req.QuotaLimit
		}
	})
	if err != nil {
		resp.NotFound(w, "api key not found")
		return
	}
	if invalidQuota {
		resp.BadRequest(w, "quota_limit 不能为负数")
		return
	}
	// 重置已用额度（quota_used 清零 + 删 Redis 实时键，下次检查按 DB=0 回填）
	if req.ResetQuota != nil && *req.ResetQuota {
		if err := h.Op.ResetApiKeyQuota(userID, id); err != nil {
			resp.Internal(w, "reset quota failed")
			return
		}
		h.Quota.Reset(r.Context(), id)
	}
	middleware.Audit(h.Op, userID, middleware.AuditEventKeyUpdate,
		"prefix="+key.Prefix, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, key)
}

// Reveal 查看 key 明文（AES-GCM 解密；明文不落日志，仅审计事件留痕）。
func (h *ApiKeyHandler) Reveal(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	userID := middleware.UserID(r.Context())
	key, err := h.Op.GetApiKey(userID, id)
	if err != nil {
		resp.NotFound(w, "api key not found")
		return
	}
	if len(key.KeyEnc) == 0 {
		resp.BadRequest(w, "该 Key 创建于旧版本（未存加密明文），无法查看，请删除后重建")
		return
	}
	plain, err := crypto.Decrypt(key.KeyEnc)
	if err != nil {
		resp.Internal(w, "decrypt key failed")
		return
	}
	middleware.Audit(h.Op, userID, middleware.AuditEventKeyReveal,
		"prefix="+key.Prefix, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"api_key": string(plain)})
}

func (h *ApiKeyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	if err := h.Op.DeleteApiKey(middleware.UserID(r.Context()), id); err != nil {
		resp.NotFound(w, "api key not found")
		return
	}
	middleware.Audit(h.Op, middleware.UserID(r.Context()), middleware.AuditEventKeyDelete,
		strconv.FormatInt(id, 10), middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"deleted": true})
}

// validIPWhitelist 校验逗号分隔 IP/CIDR 格式（空串合法 = 不限制）。
func validIPWhitelist(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if net.ParseIP(item) == nil {
			if _, _, err := net.ParseCIDR(item); err != nil {
				return false
			}
		}
	}
	return true
}

// normalizeModelLimit 去空白、去空项后按逗号重组。
func normalizeModelLimit(s string) string {
	var parts []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			parts = append(parts, m)
		}
	}
	return strings.Join(parts, ",")
}

// normalizeProviderLimit 校验并规范化渠道白名单：逗号分隔的本人渠道 ID，
// 去重保序；非数字/非正数/不属于当前用户的 ID 一律拒绝（空串 = 不限）。
// 存 ID 而非渠道名 —— 渠道改名不影响绑定；渠道删除/禁用后绑定自然失效。
func (h *ApiKeyHandler) normalizeProviderLimit(userID int64, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	ps, err := h.Op.ListProviders(userID)
	if err != nil {
		return "", fmt.Errorf("load providers failed: %w", err)
	}
	owned := make(map[int64]bool, len(ps))
	for _, p := range ps {
		owned[p.ID] = true
	}
	return normalizeProviderLimitIDs(raw, owned)
}

// normalizeProviderLimitIDs 白名单字符串的纯函数部分（owned = 当前用户拥有的渠道 ID）。
func normalizeProviderLimitIDs(raw string, owned map[int64]bool) (string, error) {
	seen := make(map[int64]bool)
	ids := make([]string, 0, 4)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, err := strconv.ParseInt(item, 10, 64)
		if err != nil || id <= 0 {
			return "", fmt.Errorf("渠道 ID 无效：%s", item)
		}
		if !owned[id] {
			return "", fmt.Errorf("渠道不存在或不属于当前用户：%s", item)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	return strings.Join(ids, ","), nil
}
