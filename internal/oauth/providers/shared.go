package providers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// shared.go adapters 共用的小工具。

// cryptoRandRead crypto/rand 读包装（错误时不 panic）。
func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

// doHTTP 带 context 的请求执行（cb.HTTPClient 可注入）。
func doHTTP(_ context.Context, cb oauth.Callbacks, req *http.Request) (*http.Response, error) {
	client := cb.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

// nowFunc 当前时间（cb.Now 可注入；默认 time.Now）。
func nowFunc(cb oauth.Callbacks) time.Time {
	if cb.Now != nil {
		return cb.Now()
	}
	return time.Now()
}

// newTokenSet 从 token endpoint 响应组装 TokenSet（expires_in 秒 → 绝对过期时间）。
func newTokenSet(access, refresh string, expiresIn int, extra map[string]string, now time.Time) *oauth.TokenSet {
	ts := &oauth.TokenSet{
		AccessToken:  access,
		RefreshToken: refresh,
		Extra:        extra,
	}
	if expiresIn > 0 {
		ts.ExpiresAt = now.Add(time.Duration(expiresIn) * time.Second)
	}
	return ts
}

// defaultNeedsRefresh 默认提前量判定：expires_at - lead 已到（或已过期）→ true。
// 无过期时间（expires_at 为零值）的 token 不触发刷新。
func defaultNeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	if tok == nil || tok.ExpiresAt.IsZero() {
		return false
	}
	return time.Until(tok.ExpiresAt) <= lead
}

// decodeJSON 限制大小的 json 解码。
func decodeJSON(r io.Reader, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r, 256<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}
