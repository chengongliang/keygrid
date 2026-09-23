package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// xai.go xAI/Grok OAuth 适配器。
//
// xAI 的 Grok Build 登录使用 OIDC discovery + RFC 8628 device_code：
//   - discovery: https://auth.x.ai/.well-known/openid-configuration
//   - scope: openid profile email offline_access grok-cli:access api:access
//   - 上游聊天默认走 cli-chat-proxy.grok.com 的 Responses API
const (
	xaiClientID            = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiScope               = "openid profile email offline_access grok-cli:access api:access"
	xaiDeviceCodeGrant     = "urn:ietf:params:oauth:grant-type:device_code"
	xaiDiscoveryDefaultURL = "https://auth.x.ai/.well-known/openid-configuration"
	xaiTokenEndpointKey    = "token_endpoint"
)

var xaiDiscoveryURL = envOr("XAI_DISCOVERY_URL", xaiDiscoveryDefaultURL)

// XAI xAI Grok device_code adapter。
type XAI struct{}

func (XAI) Key() string          { return "xai" }
func (XAI) Flow() oauth.FlowType { return oauth.FlowDeviceCode }

// xaiDiscovery OIDC discovery 只信任 HTTPS x.ai 域名返回的 endpoint，防止
// 恶意 discovery 响应把凭据交换/刷新引向任意站点。
func (XAI) xaiDiscovery(ctx context.Context, cb oauth.Callbacks) (string, string, error) {
	discoveryURL, err := validateXAIEndpoint(xaiDiscoveryURL, "discovery_url")
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("xai discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return "", "", fmt.Errorf("xai discovery request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return "", "", fmt.Errorf("xai discovery response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("xai discovery failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", fmt.Errorf("xai discovery response: %w", err)
	}
	deviceEndpoint, err := validateXAIEndpoint(data.DeviceAuthorizationEndpoint, "device_authorization_endpoint")
	if err != nil {
		return "", "", err
	}
	tokenEndpoint, err := validateXAIEndpoint(data.TokenEndpoint, "token_endpoint")
	if err != nil {
		return "", "", err
	}
	return deviceEndpoint, tokenEndpoint, nil
}

// validateXAIEndpoint 校验 discovery 返回的 OAuth endpoint。
func validateXAIEndpoint(raw, field string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("xai discovery %s is empty", field)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("xai discovery %s must be an https x.ai endpoint", field)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host != "x.ai" && !strings.HasSuffix(host, ".x.ai") {
		return "", fmt.Errorf("xai discovery %s host %q is not on x.ai", field, host)
	}
	return raw, nil
}

// BeginAuth 请求 xAI device_code，并把 token endpoint 暂存到 oauth_states.temp。
func (XAI) BeginAuth(ctx context.Context, cb oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	deviceEndpoint, tokenEndpoint, err := (XAI{}).xaiDiscovery(ctx, cb)
	if err != nil {
		return nil, nil, err
	}
	form := url.Values{
		"client_id": {xaiClientID},
		"scope":     {xaiScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("xai device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, nil, fmt.Errorf("xai device code request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, nil, fmt.Errorf("xai device code response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("xai device code request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil, fmt.Errorf("xai device code response: %w", err)
	}
	if data.DeviceCode == "" || data.UserCode == "" {
		return nil, nil, fmt.Errorf("xai device code response missing device_code/user_code")
	}
	if data.VerificationURI == "" && data.VerificationURIComplete == "" {
		return nil, nil, fmt.Errorf("xai device code response missing verification uri")
	}
	verificationURI := data.VerificationURI
	if verificationURI == "" {
		verificationURI = data.VerificationURIComplete
	}
	completeURI := data.VerificationURIComplete
	if completeURI == "" {
		completeURI = addQuery(verificationURI, "user_code", data.UserCode)
	}
	interval := data.Interval
	if interval <= 0 {
		interval = 5
	}
	return &oauth.BeginResult{
			UserCode:            data.UserCode,
			VerificationURI:     verificationURI,
			VerificationURIComp: completeURI,
			Interval:            interval,
			DeviceCodeExpiresIn: data.ExpiresIn,
		}, map[string]string{
			"device_code":       data.DeviceCode,
			xaiTokenEndpointKey: tokenEndpoint,
		}, nil
}

// Resolve 单次轮询 device_code；未完成时返回 PendingError，由宿主继续轮询。
func (XAI) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	deviceCode := strings.TrimSpace(temp["device_code"])
	if deviceCode == "" {
		return nil, fmt.Errorf("xai resolve: device_code missing")
	}
	tokenEndpoint := strings.TrimSpace(temp[xaiTokenEndpointKey])
	var err error
	if tokenEndpoint == "" {
		_, tokenEndpoint, err = (XAI{}).xaiDiscovery(ctx, cb)
		if err != nil {
			return nil, err
		}
	}
	if tokenEndpoint, err = validateXAIEndpoint(tokenEndpoint, "token_endpoint"); err != nil {
		return nil, err
	}
	if tokenEndpoint == "" {
		return nil, fmt.Errorf("xai resolve: token endpoint missing")
	}
	form := url.Values{
		"grant_type":  {xaiDeviceCodeGrant},
		"device_code": {deviceCode},
		"client_id":   {xaiClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("xai token poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, fmt.Errorf("xai token poll request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("xai token poll response: %w", err)
	}
	var data xaiTokenResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("xai token poll response: %w", err)
	}
	if data.Error == "authorization_pending" || data.Error == "slow_down" {
		return nil, &oauth.PendingError{Reason: data.Error}
	}
	if data.Error != "" {
		return nil, fmt.Errorf("xai token poll failed (%d): %s: %s", resp.StatusCode, data.Error, data.ErrorDescription)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || data.AccessToken == "" {
		return nil, fmt.Errorf("xai token poll failed (%d): access_token missing", resp.StatusCode)
	}
	extra := xaiTokenExtra(tokenEndpoint, data.IDToken, nil)
	return newTokenSet(data.AccessToken, data.RefreshToken, data.ExpiresIn, extra, nowFunc(cb)), nil
}

// Refresh 使用 form body 刷新 token；xAI 可能轮换 refresh_token，空值时保留旧值。
func (XAI) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || strings.TrimSpace(tok.RefreshToken) == "" {
		return nil, fmt.Errorf("xai refresh: no refresh_token")
	}
	tokenEndpoint := ""
	var err error
	if tok.Extra != nil {
		tokenEndpoint = strings.TrimSpace(tok.Extra[xaiTokenEndpointKey])
	}
	if tokenEndpoint == "" {
		_, tokenEndpoint, err = (XAI{}).xaiDiscovery(ctx, cb)
		if err != nil {
			return nil, err
		}
	}
	if tokenEndpoint, err = validateXAIEndpoint(tokenEndpoint, "token_endpoint"); err != nil {
		return nil, err
	}
	if tokenEndpoint == "" {
		return nil, fmt.Errorf("xai refresh: token endpoint missing")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {xaiClientID},
		"refresh_token": {tok.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	var data xaiTokenResponse
	_ = json.Unmarshal(body, &data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || data.AccessToken == "" {
		return nil, &oauth.RefreshError{Body: string(body), Status: resp.StatusCode, OAuthError: data.Error}
	}
	refreshToken := data.RefreshToken
	if refreshToken == "" {
		refreshToken = tok.RefreshToken
	}
	extra := xaiTokenExtra(tokenEndpoint, data.IDToken, tok.Extra)
	return newTokenSet(data.AccessToken, refreshToken, data.ExpiresIn, extra, nowFunc(cb)), nil
}

func (XAI) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}

type xaiTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func xaiTokenExtra(endpoint, idToken string, previous map[string]string) map[string]string {
	extra := mergeExtra(previous, nil)
	extra[xaiTokenEndpointKey] = endpoint
	if email, subject := parseXAIIdentity(idToken); email != "" {
		extra["email"] = email
		if subject != "" {
			extra["subject"] = subject
		}
	} else if subject := parseXAISubject(idToken); subject != "" {
		extra["subject"] = subject
	}
	return extra
}

func parseXAIIdentity(idToken string) (string, string) {
	if idToken == "" {
		return "", ""
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", ""
		}
	}
	var claims struct {
		Email   string `json:"email"`
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", ""
	}
	return strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Subject)
}

func parseXAISubject(idToken string) string {
	_, subject := parseXAIIdentity(idToken)
	return subject
}

func addQuery(raw, key, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}
