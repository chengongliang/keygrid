package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"

	"github.com/redis/go-redis/v9"
)

// refresh.go 刷新调度（9router open-sse/services/tokenRefresh.js 关键机制全保留）：
//   - 提前量:     expires_at - lead 触发刷新（默认 lead 5min）
//   - 去重锁:     Redis SETNX refresh:{credential_id} + TTL，防并发刷新风暴（dedup.js）
//   - 错误分类:   invalid_grant / refresh_token_reused / invalid_request → credential.status=revoked
//   - 请求时兜底: relay 发现 token 快过期 → 同步调 RefreshCredential（也走锁）

// Scheduler 刷新调度器。
type Scheduler struct {
	Op  *op.Op
	RDB *redis.Client // nil = 无 Redis（单实例部署，跳过分布式锁）

	// Lead 提前量；0 = 5min（9router TOKEN_EXPIRY_BUFFER_MS）
	Lead time.Duration
	// LockTTL 刷新锁持有时间；0 = 30s
	LockTTL time.Duration
	// HTTPClient 请求上游 token endpoint 用（测试可注入）
	HTTPClient *http.Client
	// Now 测试注入
	Now func() time.Time
}

// NewScheduler 构造。
func NewScheduler(o *op.Op, rdb *redis.Client, lead time.Duration) *Scheduler {
	s := &Scheduler{Op: o, RDB: rdb, Lead: lead}
	if s.Lead <= 0 {
		s.Lead = 5 * time.Minute
	}
	return s
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const (
	refreshLockPrefix = "refresh:"
	defaultLockTTL    = 30 * time.Second
)

// acquireRefreshLock SETNX 去重锁；false = 已有别人在刷新。
func (s *Scheduler) acquireRefreshLock(ctx context.Context, credID int64) (func(), bool) {
	if s.RDB == nil {
		return func() {}, true // 无 Redis：单实例，直接放行
	}
	key := fmt.Sprintf("%s%d", refreshLockPrefix, credID)
	ttl := s.LockTTL
	if ttl <= 0 {
		ttl = defaultLockTTL
	}
	ok, err := s.RDB.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		// Redis 故障 → fail-open（与限流同策略：锁是优化不是正确性依赖）
		return func() {}, true
	}
	if !ok {
		return nil, false
	}
	release := func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.RDB.Del(ctx2, key).Err()
	}
	return release, true
}

// credentialToken 解密凭据为 TokenSet。kind=api_key 的凭据返回 ErrNotOAuth。
var ErrNotOAuth = errors.New("credential is not an oauth token set")

func credentialToken(enc []byte) (*model.Credential, *TokenSetData, error) {
	plain, err := crypto.Decrypt(enc)
	if err != nil {
		return nil, nil, err
	}
	var d TokenSetData
	if err := json.Unmarshal(plain, &d); err != nil {
		return nil, nil, err
	}
	if d.AccessToken == "" {
		return nil, nil, ErrNotOAuth
	}
	return nil, &d, nil
}

// TokenSetData enc_data 明文结构（access_token 字段与 TokenSet 对齐；api_key 凭据无此字段）。
type TokenSetData struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time         `json:"expires_at,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// RefreshCredential 刷新单个凭据（后台扫描 + relay 兜底共用入口）。
// 全程持锁；permanent 错误 → status=revoked；其他错误保留原 token 择机重试。
func (s *Scheduler) RefreshCredential(ctx context.Context, cred *model.Credential) error {
	_, tokData, err := credentialToken(cred.EncData)
	if err != nil {
		if errors.Is(err, ErrNotOAuth) {
			return nil // api_key 类凭据无需刷新
		}
		return err
	}
	if tokData.RefreshToken == "" {
		return nil
	}

	// 找 provider → oauth provider key
	prov, err := s.Op.GetProviderGlobal(cred.ProviderID)
	if err != nil {
		return fmt.Errorf("provider %d: %w", cred.ProviderID, err)
	}
	adapter, ok := Lookup(prov.OAuthProvider)
	if !ok {
		return fmt.Errorf("oauth provider %q not registered", prov.OAuthProvider)
	}

	// 渠道勾选代理 → token endpoint 请求走平台代理；未配置则记错择机重试
	proxyURL, err := s.proxyForProvider(prov)
	if err != nil {
		_ = s.Op.MarkCredentialError(cred.ID, err.Error())
		return err
	}

	release, ok := s.acquireRefreshLock(ctx, cred.ID)
	if !ok {
		log.Printf("[oauth] credential %d refresh in-flight elsewhere, skip", cred.ID)
		return nil
	}
	defer release()

	tok := &TokenSet{
		AccessToken:  tokData.AccessToken,
		RefreshToken: tokData.RefreshToken,
		ExpiresAt:    tokData.ExpiresAt,
		Extra:        tokData.Extra,
	}

	cb := Callbacks{HTTPClient: s.clientFor(proxyURL), PublicBaseURL: ""}
	newTok, err := adapter.Refresh(ctx, cb, tok)
	if err != nil {
		permanent, code := ClassifyRefreshError(err)
		if permanent {
			// 9router: 不可恢复 → revoked，通知用户重新授权
			_ = s.Op.MarkCredentialRevoked(cred.ID, "refresh unrecoverable: "+code)
			log.Printf("[oauth] credential %d (%s) PERMANENT refresh failure (%s) → revoked", cred.ID, prov.OAuthProvider, code)
		} else {
			_ = s.Op.MarkCredentialError(cred.ID, err.Error())
			log.Printf("[oauth] credential %d (%s) refresh failed (retryable): %v", cred.ID, prov.OAuthProvider, err)
		}
		return err
	}

	// 换新 → 落库（锁内原子：refresh_token 单次使用型 provider 必须先落库再放锁）
	if err := s.saveRefreshedCredential(cred, newTok, prov.OAuthProvider); err != nil {
		return err
	}
	log.Printf("[oauth] credential %d (%s) refreshed, expires_at=%s", cred.ID, prov.OAuthProvider, newTok.ExpiresAt.Format(time.RFC3339))
	return nil
}

// saveRefreshedCredential 新 TokenSet 加密回写 credentials。
func (s *Scheduler) saveRefreshedCredential(cred *model.Credential, tok *TokenSet, providerKey string) error {
	plain, err := json.Marshal(TokenSetData{
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
	now := s.now()
	cred.EncData = enc
	cred.Status = "active"
	cred.LastError = ""
	cred.LastRefreshAt = &now
	if !tok.ExpiresAt.IsZero() {
		cred.ExpiresAt = &tok.ExpiresAt
	}
	return s.Op.UpdateCredential(cred)
}

// httpClient HTTP client（带超时）。
func (s *Scheduler) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// proxyForProvider 渠道应使用的代理地址：未勾选代理 → ""；勾选代理 → 平台
// proxy_url（后台刷新路径直查设置，不依赖 relay 的 TTL 缓存）。未配置返回错误。
func (s *Scheduler) proxyForProvider(prov *model.Provider) (string, error) {
	if !prov.UseProxy {
		return "", nil
	}
	proxyURL, err := s.Op.GetSetting(model.SettingProxyURL)
	if err != nil {
		return "", fmt.Errorf("provider %d: read proxy setting: %w", prov.ID, err)
	}
	if proxyURL == "" {
		return "", fmt.Errorf("provider %d (%s) requires proxy, but platform proxy is not configured", prov.ID, prov.OAuthProvider)
	}
	return proxyURL, nil
}

// clientFor 按代理地址返回 token 请求 client；测试注入的 HTTPClient 仅对直连生效。
func (s *Scheduler) clientFor(proxyURL string) *http.Client {
	if proxyURL == "" {
		return s.httpClient()
	}
	c, err := httpx.Client(30*time.Second, proxyURL)
	if err != nil {
		log.Printf("[oauth] build proxy client (%s) failed: %v, fallback direct", proxyURL, err)
		return s.httpClient()
	}
	return c
}

// RefreshWithBailout relay 请求时兜底刷新入口：复用锁与错误分类。
// proxyURL 为渠道应使用的平台代理地址（"" = 直连；由调用方按渠道 use_proxy 决定）。
// 成功返回新 TokenSet（已落库）；permanent 失败时凭据已置 revoked。
func (s *Scheduler) RefreshWithBailout(ctx context.Context, cred *model.Credential, providerKey string, tok *TokenSet, proxyURL string) (*TokenSet, error) {
	adapter, ok := Lookup(providerKey)
	if !ok {
		return nil, ErrNotFound
	}
	release, ok := s.acquireRefreshLock(ctx, cred.ID)
	if !ok {
		// 别的请求正在刷新：返回当前旧 token（可能马上又过期，但避免风暴）
		return tok, nil
	}
	defer release()

	cb := Callbacks{HTTPClient: s.clientFor(proxyURL)}
	newTok, err := adapter.Refresh(ctx, cb, tok)
	if err != nil {
		permanent, code := ClassifyRefreshError(err)
		if permanent {
			_ = s.Op.MarkCredentialRevoked(cred.ID, "refresh unrecoverable: "+code)
		} else {
			_ = s.Op.MarkCredentialError(cred.ID, err.Error())
		}
		return nil, err
	}
	if err := s.saveRefreshedCredential(cred, newTok, providerKey); err != nil {
		return nil, err
	}
	return newTok, nil
}

// ScanOnce 扫描一轮：expires_at < now+lead AND status=active AND refresh_token 非空的凭据。
// 返回处理的凭据数。由 internal/task 每分钟调用。
func (s *Scheduler) ScanOnce(ctx context.Context) (int, error) {
	creds, err := s.Op.ListRefreshDueCredentials(s.now().Add(s.Lead))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range creds {
		if err := s.RefreshCredential(ctx, &c); err != nil {
			// 单个失败不影响其余（错误已分类落库）
			continue
		}
		n++
	}
	return n, nil
}
