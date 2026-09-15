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
//
// PAT 备选（pt-...）：kind=api_key 直填即可（api3 需要 COSY 签名，走 qoder 平台转发不在本任务范围）。
var (
	qoderDeviceTokenURL = envOr("QODER_DEVICE_TOKEN_URL", "https://openapi.qoder.sh/api/v1/deviceToken/poll")
	qoderLoginURL       = envOr("QODER_LOGIN_URL", "https://qoder.com/device/selectAccounts")
)

// Qoder Qoder device_token 适配器。
type Qoder struct{}

func (Qoder) Key() string          { return "qoder" }
func (Qoder) Flow() oauth.FlowType { return oauth.FlowDeviceCode }

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
func (Qoder) BeginAuth(ctx context.Context, cb oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
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
		VerificationURI:     qoderLoginURL,
		VerificationURIComp: qoderLoginURL + "?" + params.Encode(),
		Interval:            2, // 9router OAuth modal 2s 轮询
	}, temp, nil
}

// Resolve 单次轮询 deviceToken/poll（9router pollDeviceToken 1:1）。
// 202/404 = pending；200+token = ok；其他 = 终态错误。
func (Qoder) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	nonce, verifier := temp["nonce"], temp["code_verifier"]
	if nonce == "" || verifier == "" {
		return nil, fmt.Errorf("qoder resolve: nonce/verifier missing")
	}
	u := qoderDeviceTokenURL + "?" + url.Values{
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
		return nil, fmt.Errorf("qoder device token poll failed (%d): %s", resp.StatusCode, string(body))
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
	expiresAt := qoderParseExpiry(data.ExpiresAt, data.ExpiresIn, nowFunc(cb))
	extra := map[string]string{}
	if data.UserID != "" {
		extra["userId"] = data.UserID
	}
	extra["machineId"] = temp["machine_id"]
	return &oauth.TokenSet{
		AccessToken:  data.Token,
		RefreshToken: data.RefreshToken,
		ExpiresAt:    expiresAt,
		Extra:        extra,
	}, nil
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

// Refresh 上游 refresh 端点对 device-token 流程返回 403（9router 实测注释）——
// 无法刷新 → 按不可恢复处理，凭据 revoked 后由用户重新授权。
func (Qoder) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("qoder refresh: no refresh_token (device tokens cannot be refreshed; re-auth required)")
	}
	// 尝试 center.qoder.sh refresh（9router 同路径）——大概率 403，按错误分类走
	refreshURL := envOr("QODER_REFRESH_URL", "https://center.qoder.sh/algo/api/v3/user/refresh_token")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.RefreshToken)

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, &oauth.RefreshError{Body: string(body), Status: resp.StatusCode, OAuthError: "refresh_not_supported"}
}

// NeedsRefresh Qoder 默认提前量。
func (Qoder) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}
