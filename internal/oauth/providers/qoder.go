package providers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

// qoder.go port of 9router src/lib/oauth/services/qoder.js（device-token 变体流程）。
//
// 与标准 RFC device_code 不同：无 device_authorization 端点，全部本地生成——
//  1. 本地生成 PKCE pair + nonce + machine_id
//  2. 浏览器打开 https://qoder.com/device/selectAccounts?challenge=...&nonce=...&machine_id=...
//  3. 轮询 GET openapi.qoder.sh/api/v1/deviceToken/poll?nonce=...&verifier=...&challenge_method=S256
//     - 202/404 = pending；200 + {token} = 成功（dt-... token，约 30 天）
//
// 上游 refresh 端点对本流程返回 403（9router 实测）→ Refresh 返回不可恢复错误，
// 过期后用户需重新授权。
var (
	qoderDeviceTokenURL   = envOr("QODER_DEVICE_TOKEN_URL", "https://openapi.qoder.sh/api/v1/deviceToken/poll")
	qoderUserInfoURL      = envOr("QODER_USERINFO_URL", "https://openapi.qoder.sh/api/v1/userinfo")
	qoderLoginURL         = envOr("QODER_LOGIN_URL", "https://qoder.com/device/selectAccounts")
	qoderCNDeviceTokenURL = envOr("QODER_CN_DEVICE_TOKEN_URL", "https://openapi.qoder.com.cn/api/v1/deviceToken/poll")
	qoderCNUserInfoURL    = envOr("QODER_CN_USERINFO_URL", "https://openapi.qoder.com.cn/api/v1/userinfo")
	qoderCNLoginURL       = envOr("QODER_CN_LOGIN_URL", "https://qoder.com.cn/device/selectAccounts")
)

// Qoder Qoder device_token 适配器。
type Qoder struct{}
type QoderCN struct{}

func (Qoder) Key() string            { return "qoder" }
func (Qoder) Flow() oauth.FlowType   { return oauth.FlowDeviceCode }
func (QoderCN) Key() string          { return "qoder-cn" }
func (QoderCN) Flow() oauth.FlowType { return oauth.FlowDeviceCode }

// qoderUUID uuid v4（9router: uuidv4()）。
func qoderUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// qoderPkcePair 32 字节 verifier + S256 challenge（9router generatePkcePair）。
func qoderPkcePair() (verifier, challenge string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		b = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// BeginAuth 本地生成 PKCE+nonce+machineId，拼 device 登录 URL（9router initiateDeviceFlow）。
func (Qoder) BeginAuth(_ context.Context, _ oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	return qoderBeginAuth(qoderLoginURL)
}

func (QoderCN) BeginAuth(_ context.Context, _ oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	return qoderBeginAuth(qoderCNLoginURL)
}

func qoderBeginAuth(loginURL string) (*oauth.BeginResult, map[string]string, error) {
	verifier, challenge := qoderPkcePair()
	nonce := qoderUUID()
	machineID := qoderUUID()

	params := url.Values{
		"challenge":        {challenge},
		"challenge_method": {"S256"},
		"machine_id":       {machineID},
		"nonce":            {nonce},
	}
	temp := map[string]string{"nonce": nonce, "code_verifier": verifier, "machine_id": machineID}
	return &oauth.BeginResult{
		UserCode:            strings.ToUpper(nonce[:8]),
		VerificationURI:     loginURL,
		VerificationURIComp: loginURL + "?" + params.Encode(),
		Interval:            2,   // 9router OAuth modal 2s 轮询
		DeviceCodeExpiresIn: 300, // 9router device flow timeout
	}, temp, nil
}

// Resolve 单次轮询 deviceToken/poll（9router pollDeviceToken 1:1）。
// 202/404 = pending；200+token = ok；其他 = 终态错误。
func (Qoder) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	return qoderResolve(ctx, cb, temp, qoderDeviceTokenURL, qoderUserInfoURL)
}

func (QoderCN) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	return qoderResolve(ctx, cb, temp, qoderCNDeviceTokenURL, qoderCNUserInfoURL)
}

func qoderResolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string, deviceTokenURL, userInfoURL string) (*oauth.TokenSet, error) {
	nonce, verifier := temp["nonce"], temp["code_verifier"]
	if nonce == "" || verifier == "" {
		return nil, fmt.Errorf("qoder resolve: nonce/verifier missing")
	}
	u := deviceTokenURL + "?" + url.Values{
		"nonce":            {nonce},
		"verifier":         {verifier},
		"challenge_method": {"S256"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0") // 9router 同款 UA

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// pending：server 已注册 device code 但用户还没完成浏览器流程
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNotFound {
		return nil, &oauth.PendingError{Reason: "authorization_pending"}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		var errorBody struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &errorBody) == nil && strings.TrimSpace(errorBody.Message) != "" {
			return nil, fmt.Errorf("qoder device token poll failed: %s", strings.TrimSpace(errorBody.Message))
		}
		return nil, fmt.Errorf("qoder device token poll failed (%d)", resp.StatusCode)
	}

	var data struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		ExpiresAt    any    `json:"expires_at"`
		ExpiresIn    any    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("qoder poll response: %w", err)
	}
	if data.Token == "" {
		return nil, fmt.Errorf("qoder device token poll returned 200 but no token")
	}
	extra := qoderFetchUserInfoAt(ctx, cb, data.Token, userInfoURL)
	if extra == nil {
		extra = map[string]string{}
	}
	now := nowFunc(cb)
	expiresAt := qoderParseExpiry(data.ExpiresAt, data.ExpiresIn, now)
	if minExpiry := now.Add(24 * time.Hour); expiresAt.Before(minExpiry) {
		expiresAt = minExpiry
	}
	if data.UserID != "" {
		extra["userId"] = data.UserID
	}
	extra["machineId"] = temp["machine_id"]
	extra["accountIdentity"] = qoderAccountIdentity(extra["email"], data.UserID)
	return &oauth.TokenSet{
		AccessToken:  data.Token,
		RefreshToken: data.RefreshToken,
		ExpiresAt:    expiresAt,
		Extra:        extra,
	}, nil
}

// qoderFetchUserInfo 登录成功后的资料读取是 best-effort，失败不能阻断授权。
func qoderFetchUserInfo(ctx context.Context, cb oauth.Callbacks, accessToken string) map[string]string {
	return qoderFetchUserInfoAt(ctx, cb, accessToken, qoderUserInfoURL)
}

func qoderFetchUserInfoAt(ctx context.Context, cb oauth.Callbacks, accessToken, userInfoURL string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var profile struct {
		Name           string `json:"name"`
		Username       string `json:"username"`
		Email          string `json:"email"`
		OrganizationID string `json:"organization_id"`
	}
	if err := decodeJSON(resp.Body, &profile); err != nil {
		return nil
	}
	name := strings.TrimSpace(profile.Name)
	if name == "" {
		name = strings.TrimSpace(profile.Username)
	}
	extra := map[string]string{}
	if name != "" {
		extra["displayName"] = name
	}
	if email := strings.TrimSpace(profile.Email); email != "" {
		extra["email"] = email
		extra["accountIdentity"] = qoderAccountIdentity(email, "")
	}
	if organizationID := strings.TrimSpace(profile.OrganizationID); organizationID != "" {
		extra["organizationId"] = organizationID
	}
	return extra
}

// qoderAccountIdentity 规范化账号身份：邮箱优先，资料接口失败时以稳定 user_id 去重。
func qoderAccountIdentity(email, userID string) string {
	if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
		return "email:" + email
	}
	if userID = strings.TrimSpace(userID); userID != "" {
		return "qoder-user:" + userID
	}
	return ""
}

// qoderParseExpiry 上游过期时间 → 绝对时间（9router QoderService.parseExpiry 1:1）。
// 接受：数字 ms epoch / 数字字符串 ms epoch / RFC3339 / expires_in 秒；兜底 now+30 天。
func qoderParseExpiry(expiresAt, expiresInSeconds any, now time.Time) time.Time {
	switch v := expiresAt.(type) {
	case float64:
		if v > 0 {
			return time.UnixMilli(int64(v))
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed != "" {
			// 纯数字字符串 → ms epoch（不能让 Date.parse 类逻辑吞掉 "2026" 这种短数字）
			if isAllDigits(trimmed) {
				var ms int64
				if _, err := fmt.Sscanf(trimmed, "%d", &ms); err == nil && ms > 0 {
					return time.UnixMilli(ms)
				}
			}
			if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
				return t
			}
		}
	}
	if sec, ok := expiresInSeconds.(float64); ok && sec >= 0 {
		return now.Add(time.Duration(sec) * time.Second)
	}
	return now.Add(30 * 24 * time.Hour)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Refresh Qoder 的 device token 不支持 refresh；过期后需要用户重新授权。
func (Qoder) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	return qoderRefresh(tok)
}

func (QoderCN) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	return qoderRefresh(tok)
}

func qoderRefresh(tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("qoder refresh: no refresh_token (device tokens cannot be refreshed; re-auth required)")
	}
	return nil, &oauth.RefreshError{OAuthError: "refresh_not_supported"}
}

// NeedsRefresh Qoder 默认提前量。
func (Qoder) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}

func (QoderCN) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}
