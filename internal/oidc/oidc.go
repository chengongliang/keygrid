package oidc

// Discovery: 缓存 {issuer}/.well-known/openid-configuration。
// OIDC Discovery 1.0：metadata.issuer 必须与配置 issuer 一致（防混叠攻击）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProviderMetadata /.well-known/openid-configuration 的关键字段子集。
type ProviderMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	EndSessionEndpoint    string   `json:"end_session_endpoint"`
	IDTokenSigningAlgVals []string `json:"id_token_signing_alg_values_supported"`
}

type discoveryCache struct {
	mu    sync.Mutex
	meta  *ProviderMetadata
	fetch time.Time
}

// Client OIDC 客户端：discovery + JWKS 缓存、token 交换、id_token 校验。
type Client struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	HTTPClient   *http.Client

	disc   discoveryCache
	jwksMu sync.Mutex
	jwks   *jwksSet
	// jwksFetchedAt 上次拉取时间；keyFor 的 TTL 判定用
	jwksFetchedAt time.Time
}

func NewClient(issuer, clientID, clientSecret string, scopes []string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		Issuer:       strings.TrimRight(issuer, "/"),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       scopes,
		HTTPClient:   hc,
	}
}

// Metadata 发现并缓存 openid-configuration；TTL 内复用缓存。
func (c *Client) Metadata(ctx context.Context) (*ProviderMetadata, error) {
	c.disc.mu.Lock()
	defer c.disc.mu.Unlock()
	if c.disc.meta != nil && time.Since(c.disc.fetch) < 10*time.Minute {
		return c.disc.meta, nil
	}
	u := c.Issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: status %d", resp.StatusCode)
	}
	var meta ProviderMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("oidc discovery decode: %w", err)
	}
	// 标准要求 metadata.issuer == 配置 issuer
	if meta.Issuer != c.Issuer {
		return nil, fmt.Errorf("oidc discovery: issuer mismatch (config %q != metadata %q)", c.Issuer, meta.Issuer)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" || meta.JWKSURI == "" {
		return nil, fmt.Errorf("oidc discovery: missing required endpoints")
	}
	c.disc.meta = &meta
	c.disc.fetch = time.Now()
	return c.disc.meta, nil
}

// EndSession RP-Initiated Logout 用；IdP 不支持时返回空。
func (c *Client) EndSession(ctx context.Context) string {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return ""
	}
	return meta.EndSessionEndpoint
}
