package relay

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// oauth.go relay 侧 OAuth 渠道支持：
//   - route:    buildCandidates 现在放行 kind=oauth 且凭据解密成功的渠道（route_oauth.go）
//   - credential: api_key 凭据 → Bearer；oauth 凭据 → access_token
//   - 兜底刷新:  token 即将过期（<lead）→ 请求时同步刷新（走 Scheduler 锁），失败熔断该渠道
//   - revoked:   渠道跳过（handler 已判）

// credentialSecret 返回上游鉴权密钥（api_key 或 oauth access_token）及 token extra
// （codex 需要 extra.chatgptAccountId 拼 chatgpt-account-id 头）。
// oauth token 临期时先同步兜底刷新（9router 请求时兜底机制）。
// 返回 (secret, extra, credential status, err)。
func (h *Handler) credentialSecret(ctx context.Context, cred *model.Credential, p *model.Provider) (string, map[string]string, string, error) {
	if cred.Status == "revoked" {
		return "", nil, cred.Status, errors.New("credential revoked")
	}

	plain, err := crypto.Decrypt(cred.EncData)
	if err != nil {
		return "", nil, cred.Status, err
	}
	var d oauth.TokenSetData
	if err := json.Unmarshal(plain, &d); err != nil || d.AccessToken == "" {
		// api_key 类凭据
		var kd struct {
			APIKey string `json:"api_key"`
		}
		if err2 := json.Unmarshal(plain, &kd); err2 != nil || kd.APIKey == "" {
			return "", nil, cred.Status, errors.New("invalid credential data")
		}
		return kd.APIKey, nil, cred.Status, nil
	}

	// oauth 类凭据：临期兜底刷新
	if d.RefreshToken != "" && tokenNeedsBailout(d.ExpiresAt, h.oauthLead()) {
		newTok, err := h.bailoutRefresh(ctx, cred, p, &d)
		if err != nil {
			// 刷新失败：permanent → 凭据 revoked（RefreshCredential 内已落库），本次跳过
			return "", nil, cred.Status, err
		}
		return newTok.AccessToken, newTok.Extra, "active", nil
	}

	if d.AccessToken == "" {
		return "", nil, cred.Status, errors.New("empty access token")
	}
	return d.AccessToken, d.Extra, cred.Status, nil
}

// tokenNeedsBailout 提前量判定（与 oauth.Scheduler 同 lead）。
func tokenNeedsBailout(expiresAt time.Time, lead time.Duration) bool {
	if expiresAt.IsZero() {
		return false
	}
	return time.Until(expiresAt) <= lead
}

// oauthLead 提前量（Handler 注入；默认 5min）。
func (h *Handler) oauthLead() time.Duration {
	if h.OAuthLead > 0 {
		return h.OAuthLead
	}
	return 5 * time.Minute
}

// bailoutRefresh 请求时兜底刷新（走 Scheduler：Redis 锁 + 错误分类全复用）。
// 渠道勾选代理时用平台代理发 token endpoint 请求；代理未配置则直接报错
// （转发主循环会 failover 该渠道）。
func (h *Handler) bailoutRefresh(ctx context.Context, cred *model.Credential, p *model.Provider, d *oauth.TokenSetData) (*oauth.TokenSet, error) {
	if h.OAuth == nil {
		return nil, errors.New("oauth scheduler not configured")
	}
	proxyURL := ""
	if p.UseProxy {
		if h.Op == nil {
			return nil, errors.New("channel " + p.Name + " requires proxy, but platform proxy is not configured")
		}
		v, err := h.Op.ProxyURL()
		if err != nil {
			return nil, err
		}
		if v == "" {
			return nil, errors.New("channel " + p.Name + " requires proxy, but platform proxy is not configured")
		}
		proxyURL = v
	}
	tok := &oauth.TokenSet{
		AccessToken:  d.AccessToken,
		RefreshToken: d.RefreshToken,
		ExpiresAt:    d.ExpiresAt,
		Extra:        d.Extra,
	}
	newTok, err := h.OAuth.RefreshWithBailout(ctx, cred, p.OAuthProvider, tok, proxyURL)
	if err != nil {
		permanent, code := oauth.ClassifyRefreshError(err)
		if permanent {
			log.Printf("[relay] credential %d (%s) permanent refresh failure (%s) → revoked", cred.ID, p.OAuthProvider, code)
		}
		return nil, err
	}
	return newTok, nil
}
