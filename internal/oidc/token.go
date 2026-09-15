package oidc

// Token 交换（authorization_code + PKCE）与 authorize URL 拼接。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TokenResponse /oidc/token 响应子集。
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// ExchangeCode code 换 token（client_secret_post + code_verifier）。
func (c *Client) ExchangeCode(ctx context.Context, redirectURI, code, codeVerifier string) (*TokenResponse, error) {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"code_verifier": {codeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var te tokenError
		_ = json.NewDecoder(resp.Body).Decode(&te)
		return nil, fmt.Errorf("oidc token: status %d error=%s desc=%s", resp.StatusCode, te.Error, te.ErrorDescription)
	}
	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("oidc token decode: %w", err)
	}
	if tr.IDToken == "" {
		return nil, fmt.Errorf("oidc token: no id_token in response")
	}
	return &tr, nil
}

// AuthorizeURL 拼接 IdP /authorize 跳转 URL（含 PKCE/state/nonce）。
func (c *Client) AuthorizeURL(ctx context.Context, redirectURI, state, nonce, codeChallenge string) (string, error) {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(c.Scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return meta.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Claims id_token 便捷取值。
type Claims struct {
	Subject  string
	Email    string
	EmailVer bool
	Name     string
	Nonce    string
}

func ClaimsFromMap(m map[string]any) Claims {
	c := Claims{}
	c.Subject, _ = m["sub"].(string)
	c.Email, _ = m["email"].(string)
	switch v := m["email_verified"].(type) {
	case bool:
		c.EmailVer = v
	case string: // 部分 IdP 以字符串返回
		c.EmailVer = strings.EqualFold(v, "true") || v == "1"
	}
	c.Name, _ = m["name"].(string)
	c.Nonce, _ = m["nonce"].(string)
	return c
}
