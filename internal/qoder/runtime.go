package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// runtime.go Qoder 运行时状态：模型目录缓存、PAT→job token 缓存、userinfo 缓存。
//
// 全部按凭据隔离、只放内存（进程重启后按需重取，不做落库）。

// Runtime 进程级缓存。零值不可用，用 NewRuntime 构造。
type Runtime struct {
	mu       sync.Mutex
	catalogs map[string]cachedCatalog
	flights  map[string]*catalogFlight
	pats     map[string]cachedPAT
	users    map[string]cachedUser
	now      func() time.Time
}

// NewRuntime 构造一个缓存实例。
func NewRuntime() *Runtime {
	return &Runtime{
		catalogs: map[string]cachedCatalog{},
		flights:  map[string]*catalogFlight{},
		pats:     map[string]cachedPAT{},
		users:    map[string]cachedUser{},
		now:      time.Now,
	}
}

var defaultRuntime = NewRuntime()

// Default 进程级默认缓存（渠道面板与转发路径共享，避免重复请求上游）。
func Default() *Runtime { return defaultRuntime }

// SetNow 测试注入时钟。
func (r *Runtime) SetNow(fn func() time.Time) {
	r.mu.Lock()
	r.now = fn
	r.mu.Unlock()
}

// ---- 凭据解析 ----

// Resolved 已解析成可签名状态的凭据。
type Resolved struct {
	Region    string
	Token     string // 可签名 token：设备流 dt-… 或 PAT 兑换来的 jt-…
	UserID    string
	MachineID string
	Name      string
	Email     string
	// PAT 原始凭据（非空表示这条凭据是 PAT 直填，缓存与重换都按它做键）。
	PAT string
}

// Creds 转成 COSY 签名入参。
func (r Resolved) Creds() Creds {
	return Creds{UserID: r.UserID, Token: r.Token, Name: r.Name, Email: r.Email, MachineID: r.MachineID}
}

// cacheKey 目录缓存键：区域 + 账号 + token（同账号重新授权共用一份目录）。
func (r Resolved) cacheKey() string {
	seed := r.UserID
	if seed == "" {
		seed = r.Token
	}
	return sha256Hex([]byte("qoder:" + r.Region + ":" + seed))
}

// cachedPAT PAT 兑换出的 job token。
type cachedPAT struct {
	jobToken  string
	userID    string
	expiresAt time.Time
}

// cachedUser userinfo 结果缓存。
type cachedUser struct {
	userID string
	name   string
	email  string
}

// patRefreshBuffer job token 剩余有效期不足此值时重换。
const patRefreshBuffer = 5 * time.Minute

// patDefaultTTL 上游没给过期时间时的兜底（9router 同值 24h）。
const patDefaultTTL = 24 * time.Hour

// ResolveCredential 把渠道凭据解析成可签名状态。
//
//   - PAT（pt-…）：兑换成 jt- 并用 userinfo 取 userId（两者都缓存）；
//   - 设备流 token（dt-…）：直接用，userId 优先取 extra，缺失时调一次 userinfo
//     兜底（存量凭据常见：旧版本只存了 machineId）。
//
// extra 为凭据里落库的 extra（userId / machineId / name / email）。
func (r *Runtime) ResolveCredential(ctx context.Context, client Doer, region, token string, extra map[string]string) (Resolved, error) {
	client = doerOrDefault(client)
	region = NormalizeRegion(region)
	rc := Resolved{
		Region:    region,
		Token:     token,
		UserID:    extra["userId"],
		MachineID: extra["machineId"],
		Name:      extra["name"],
		Email:     extra["email"],
	}
	if token == "" {
		return rc, ErrMissingToken
	}

	if IsPAT(token) {
		rc.PAT = token
		cached, err := r.jobToken(ctx, client, region, token)
		if err != nil {
			return rc, err
		}
		rc.Token = cached.jobToken
		if rc.UserID == "" {
			rc.UserID = cached.userID
		}
	}

	if rc.UserID == "" {
		// 缺 userId 就无法签名：用 userinfo 兜底取一次（结果进程内缓存）。
		rc.UserID = r.userIDFor(ctx, client, region, rc.Token)
	}
	return rc, nil
}

// jobToken 取（必要时兑换）PAT 对应的 job token。
func (r *Runtime) jobToken(ctx context.Context, client Doer, region, pat string) (cachedPAT, error) {
	key := sha256Hex([]byte("qoder-pat:" + region + ":" + pat))
	now := r.now()

	r.mu.Lock()
	if c, ok := r.pats[key]; ok && c.expiresAt.Sub(now) > patRefreshBuffer {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	jobToken, expiresAt, err := exchangeJobToken(ctx, client, region, pat)
	if err != nil {
		return cachedPAT{}, err
	}
	userID := r.userIDFor(ctx, client, region, jobToken)

	entry := cachedPAT{jobToken: jobToken, userID: userID, expiresAt: expiresAt}
	r.mu.Lock()
	r.pats[key] = entry
	r.mu.Unlock()
	return entry, nil
}

// exchangeJobToken POST {openapi}/api/v1/jobToken/exchange（普通 JSON，不签名）。
func exchangeJobToken(ctx context.Context, client Doer, region, pat string) (string, time.Time, error) {
	target := JobTokenExchangeURL(region)
	ctx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()

	payload, _ := jsonMarshal(map[string]string{"personal_token": pat})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(payload)))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qodercli/1.0.0")
	req.Header.Set("Cosy-Version", IDEVersion)
	req.Header.Set("Cosy-ClientType", ClientType)
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("qoder PAT exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, &UpstreamError{Status: resp.StatusCode, Body: truncate(string(body), 256)}
	}
	var data struct {
		Token     string `json:"token"`
		ExpiresAt any    `json:"expires_at"`
		ExpiresIn any    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", time.Time{}, fmt.Errorf("qoder PAT exchange: parse response: %w", err)
	}
	if data.Token == "" {
		return "", time.Time{}, fmt.Errorf("qoder PAT exchange: upstream returned no job token")
	}
	return data.Token, parseExpiry(data.ExpiresAt, data.ExpiresIn, time.Now(), patDefaultTTL), nil
}

// userIDFor 取 token 对应的 userId（userinfo，结果缓存；失败返回空串由调用方决定语义）。
func (r *Runtime) userIDFor(ctx context.Context, client Doer, region, token string) string {
	if token == "" {
		return ""
	}
	key := sha256Hex([]byte("qoder-user:" + region + ":" + token))
	r.mu.Lock()
	if u, ok := r.users[key]; ok {
		r.mu.Unlock()
		return u.userID
	}
	r.mu.Unlock()

	info, err := FetchUserInfo(ctx, client, region, token)
	if err != nil || info.UserID == "" {
		return ""
	}
	r.mu.Lock()
	r.users[key] = cachedUser{userID: info.UserID, name: info.Name, email: info.Email}
	r.mu.Unlock()
	return info.UserID
}

// UserInfo Qoder 账号信息。
type UserInfo struct {
	UserID string
	Name   string
	Email  string
}

// FetchUserInfo GET {openapi}/api/v1/userinfo（Bearer，不签名）。
func FetchUserInfo(ctx context.Context, client Doer, region, token string) (UserInfo, error) {
	client = doerOrDefault(client)
	ctx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserInfoURL(NormalizeRegion(region)), nil)
	if err != nil {
		return UserInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qodercli/1.0.0")

	resp, err := client.Do(req)
	if err != nil {
		return UserInfo{}, fmt.Errorf("qoder userinfo: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return UserInfo{}, &UpstreamError{Status: resp.StatusCode, Body: truncate(string(body), 256)}
	}
	var data struct {
		ID       string `json:"id"`
		UserID   string `json:"userId"`
		UserID2  string `json:"user_id"`
		Name     string `json:"name"`
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return UserInfo{}, fmt.Errorf("qoder userinfo: parse response: %w", err)
	}
	id := firstNonEmpty(data.ID, data.UserID, data.UserID2)
	return UserInfo{
		UserID: id,
		Name:   strings.TrimSpace(firstNonEmpty(data.Name, data.Username)),
		Email:  strings.TrimSpace(data.Email),
	}, nil
}

// parseExpiry 上游过期时间 → 绝对时间。
//
// 接受：ms epoch（数字或纯数字字符串）、RFC3339、expires_in 秒；兜底 now+def。
// 顺序很重要：先判纯数字，避免 "2026" 这类短数字被当成年份解析。
func parseExpiry(expiresAt, expiresIn any, now time.Time, def time.Duration) time.Time {
	switch v := expiresAt.(type) {
	case float64:
		if v > 0 {
			return time.UnixMilli(int64(v))
		}
	case string:
		s := strings.TrimSpace(v)
		if s != "" {
			if isAllDigits(s) {
				var ms int64
				if _, err := fmt.Sscanf(s, "%d", &ms); err == nil && ms > 0 {
					return time.UnixMilli(ms)
				}
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	}
	if sec, ok := expiresIn.(float64); ok && sec >= 0 {
		return now.Add(time.Duration(sec) * time.Second)
	}
	return now.Add(def)
}

// isAllDigits 是否全为数字。
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

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
