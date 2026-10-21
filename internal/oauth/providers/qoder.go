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
	"github.com/chengongliang/keygrid/internal/qoder"
)

// qoder.go Qoder device-token 变体设备流（国际站 qoder.com / 国内站 qoder.com.cn）。
//
// 与标准 RFC device_code 不同：没有 device_authorization 端点，全部本地生成——
//  1. 本地生成 PKCE pair + nonce + machine_id
//  2. 浏览器打开 https://qoder.com/device/selectAccounts?challenge=...&nonce=...&machine_id=...
//  3. 轮询 GET openapi.qoder.sh/api/v1/deviceToken/poll?nonce=...&verifier=...&challenge_method=S256
//     - 202/404 = pending；200 + {token} = 成功（dt-... token，约 30 天）
//  4. 用 token 调 openapi/api/v1/userinfo 取 userId —— COSY 签名必需（9router 同款兜底）
//
// 上游 refresh 端点对本流程返回 403（9router 实测）→ Refresh 返回不可恢复错误，
// 过期后用户需重新授权。
//
// 授权之后的对话/模型列表不在 OpenAI 兼容面（api3 没有 /v1/*），走
// internal/qoder 的 COSY 签名 + 私有 /algo 协议；见 internal/relay/qoder.go。

// Qoder 国际站设备流。
type Qoder struct{}

// QoderCN 国内站设备流（qoder.com.cn，端点为 gateway.qoder.com.cn）。
type QoderCN struct{}

func (Qoder) Key() string            { return qoder.ProviderIDIntl }
func (QoderCN) Key() string          { return qoder.ProviderIDCN }
func (Qoder) Flow() oauth.FlowType   { return oauth.FlowDeviceCode }
func (QoderCN) Flow() oauth.FlowType { return oauth.FlowDeviceCode }

func (Qoder) BeginAuth(ctx context.Context, cb oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	return beginQoderDevice(qoder.RegionIntl)
}

func (QoderCN) BeginAuth(ctx context.Context, cb oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	return beginQoderDevice(qoder.RegionCN)
}

func (Qoder) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	return resolveQoderDevice(ctx, cb, qoder.RegionIntl, temp)
}

func (QoderCN) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	return resolveQoderDevice(ctx, cb, qoder.RegionCN, temp)
}

func (Qoder) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	return refreshQoder(ctx, cb, qoder.RegionIntl, tok)
}

func (QoderCN) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	return refreshQoder(ctx, cb, qoder.RegionCN, tok)
}

// NeedsRefresh 设备流 token 无法刷新：返回 false 让调度器别在临期时白跑一趟
// （过期后由上游 401 + 渠道面板提示重新授权）。
func (Qoder) NeedsRefresh(*oauth.TokenSet, time.Duration) bool   { return false }
func (QoderCN) NeedsRefresh(*oauth.TokenSet, time.Duration) bool { return false }

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

// beginQoderDevice 本地生成 PKCE+nonce+machineId，拼 device 登录 URL。
func beginQoderDevice(region string) (*oauth.BeginResult, map[string]string, error) {
	verifier, challenge := qoderPkcePair()
	nonce := qoderUUID()
	machineID := qoderUUID()

	params := url.Values{
		"challenge":        {challenge},
		"challenge_method": {"S256"},
		"machine_id":       {machineID},
		"nonce":            {nonce},
	}
	loginURL := qoder.LoginURL(region)
	temp := map[string]string{"nonce": nonce, "code_verifier": verifier, "machine_id": machineID, "region": region}
	return &oauth.BeginResult{
		VerificationURI:     loginURL,
		VerificationURIComp: loginURL + "?" + params.Encode(),
		Interval:            2, // 9router OAuth modal 2s 轮询
	}, temp, nil
}

// resolveQoderDevice 单次轮询 deviceToken/poll（9router pollDeviceToken 1:1）。
// 202/404 = pending；200+token = ok；其他 = 终态错误。
func resolveQoderDevice(ctx context.Context, cb oauth.Callbacks, region string, temp map[string]string) (*oauth.TokenSet, error) {
	nonce, verifier := temp["nonce"], temp["code_verifier"]
	if nonce == "" || verifier == "" {
		return nil, fmt.Errorf("qoder resolve: nonce/verifier missing")
	}
	u := qoder.DeviceTokenURL(region) + "?" + url.Values{
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
	now := nowFunc(cb)
	// 上游若给出已过期/极短的过期时间，按 9router 的做法兜底到至少 24 小时，
	// 避免凭据刚落地就临期（进而被兜底刷新判死）。
	expiresAt := qoderParseExpiry(data.ExpiresAt, data.ExpiresIn, now)
	if min := now.Add(24 * time.Hour); expiresAt.Before(min) {
		expiresAt = min
	}

	extra := map[string]string{
		"machineId": temp["machine_id"],
		"region":    region,
	}
	userID := data.UserID
	name, email := "", ""
	// userId 是 COSY 签名必需项：poll 响应里通常没有，这里补一次 userinfo。
	if info, ierr := qoder.FetchUserInfo(ctx, cb.HTTPClient, region, data.Token); ierr == nil {
		if info.UserID != "" {
			userID = info.UserID
		}
		name, email = info.Name, info.Email
	}
	if userID != "" {
		extra["userId"] = userID
	}
	if name != "" {
		extra["name"] = name
	}
	if email != "" {
		extra["email"] = email
	}

	return &oauth.TokenSet{
		AccessToken:  data.Token,
		RefreshToken: data.RefreshToken,
		ExpiresAt:    expiresAt,
		Extra:        extra,
	}, nil
}

// refreshQoder 设备流 token 不支持刷新：上游 refresh 端点对 dt-… 返回 403。
// 返回带 refresh_not_supported 标记的 RefreshError，分类器据此把凭据标 revoked，
// 由用户在渠道面板重新授权。
func refreshQoder(ctx context.Context, cb oauth.Callbacks, region string, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	refreshURL := qoder.RefreshURL(region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, strings.NewReader("{}"))
	if err != nil {
		return nil, &oauth.RefreshError{Status: 0, OAuthError: "refresh_not_supported", Body: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if tok != nil && tok.RefreshToken != "" {
		req.Header.Set("Authorization", "Bearer "+tok.RefreshToken)
	}

	status, body := 0, ""
	if resp, rerr := doHTTP(ctx, cb, req); rerr == nil {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		status, body = resp.StatusCode, string(raw)
	} else {
		body = rerr.Error()
	}
	return nil, &oauth.RefreshError{Body: body, Status: status, OAuthError: "refresh_not_supported"}
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
