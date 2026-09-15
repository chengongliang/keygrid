package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// anthropic.go port of 9router src/lib/oauth/providers/claude.js (Claude Pro OAuth)。
//
// Endpoints (9router open-sse/providers/registry/claude.js oauth block):
//
//	clientId:     9d1c250a-e61b-44d9-88ed-5944d1962f5e
//	authorizeUrl: https://claude.ai/oauth/authorize
//	tokenUrl:     https://api.anthropic.com/v1/oauth/token
//	scopes: org:create_api_key user:profile user:inference, S256
const claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

var (
	claudeAuthorizeURL = "https://claude.ai/oauth/authorize"
	claudeTokenURL     = "https://api.anthropic.com/v1/oauth/token"
)

// Anthropic claude oauth adapter（authorization_code + pkce）。
type Anthropic struct{}

func (Anthropic) Key() string          { return "anthropic" }
func (Anthropic) Flow() oauth.FlowType { return oauth.FlowPKCE }

// BeginAuth 9router claude buildAuthUrl：query 首参 code=true，scopes 空格分隔。
func (Anthropic) BeginAuth(ctx context.Context, cb oauth.Callbacks, state string) (*oauth.BeginResult, map[string]string, error) {
	verifier := newCodeVerifier()
	challenge := s256Challenge(verifier)

	params := url.Values{}
	params.Set("code", "true")
	params.Set("client_id", claudeClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", pkceRedirectURI(cb))
	params.Set("scope", "org:create_api_key user:profile user:inference")
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)

	temp := map[string]string{"code_verifier": verifier}
	return &oauth.BeginResult{AuthorizeURL: claudeAuthorizeURL + "?" + params.Encode()}, temp, nil
}

// Resolve 9router claude exchangeToken：JSON body；code 可能带 "#state" 后缀。
func (Anthropic) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	return anthropicResolve(ctx, cb, temp)
}

func anthropicResolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	rawCode := temp["code"]
	verifier := temp["code_verifier"]
	state := temp["state"]
	if rawCode == "" || verifier == "" {
		return nil, fmt.Errorf("anthropic resolve: code/code_verifier missing")
	}

	// 9router: code#state → authCode + codeState
	authCode := rawCode
	codeState := ""
	if idx := strings.Index(rawCode, "#"); idx >= 0 {
		authCode = rawCode[:idx]
		codeState = rawCode[idx+1:]
	}
	if codeState == "" {
		codeState = state
	}

	payload := map[string]string{
		"code":          authCode,
		"state":         codeState,
		"grant_type":    "authorization_code",
		"client_id":     claudeClientID,
		"redirect_uri":  pkceRedirectURI(cb),
		"code_verifier": verifier,
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL, strings.NewReader(string(buf)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token exchange failed: %s", string(body))
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("token response: %w", err)
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("token exchange missing access_token: %s", string(body))
	}
	extra := map[string]string{}
	if data.Scope != "" {
		extra["scope"] = data.Scope
	}
	return newTokenSet(data.AccessToken, data.RefreshToken, data.ExpiresIn, extra, nowFunc(cb)), nil
}

// Refresh JSON body、无 client_secret（9router REFRESH_PROFILES.claude：bodyFormat=json）。
func (Anthropic) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("anthropic refresh: no refresh_token")
	}
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": tok.RefreshToken,
		"client_id":     claudeClientID,
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL, strings.NewReader(string(buf)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &data)
	if data.AccessToken == "" {
		return nil, &oauth.RefreshError{Body: string(body), Status: resp.StatusCode, OAuthError: data.Error}
	}
	rt := data.RefreshToken
	if rt == "" {
		rt = tok.RefreshToken
	}
	return newTokenSet(data.AccessToken, rt, data.ExpiresIn, tok.Extra, nowFunc(cb)), nil
}

// NeedsRefresh 9router claude refreshLeadMs=14400000 (4h) —— 请求时兜底仍走通用 lead。
func (Anthropic) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}
