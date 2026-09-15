// Package httpx 上游 HTTP client 工厂：按 (timeout, proxyURL) 复用共享 client，
// 供 relay 转发 / OAuth 刷新授权 / 渠道探测三条路径统一走「渠道级出口代理」。
//
// 代理语义：管理员在系统设置统一配置 proxy_url（http/https/socks5/socks5h），
// 用户在渠道上只勾选是否启用。proxyURL 为空 = 直连（保留 ProxyFromEnvironment
// 的部署级兜底行为，与原 relay newUpstreamClient 一致）。
package httpx

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type clientKey struct {
	timeout  time.Duration
	proxyURL string
}

var (
	mu      sync.RWMutex
	clients = map[clientKey]*http.Client{}
)

// Client 返回共享 HTTP client：同 (timeout, proxyURL) 参数复用同一连接池。
// proxyURL 空 = 直连；非空 = 固定走该代理（Transport.Proxy 用固定 URL，
// socks5/socks5h 由 net/http 原生支持）。
func Client(timeout time.Duration, proxyURL string) (*http.Client, error) {
	if proxyURL != "" {
		if err := ValidateProxyURL(proxyURL); err != nil {
			return nil, err
		}
	}
	key := clientKey{timeout: timeout, proxyURL: proxyURL}

	mu.RLock()
	c := clients[key]
	mu.RUnlock()
	if c != nil {
		return c, nil
	}

	t := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	if proxyURL == "" {
		t.Proxy = http.ProxyFromEnvironment
	} else {
		u, _ := url.Parse(proxyURL) // ValidateProxyURL 已确保可解析
		t.Proxy = func(*http.Request) (*url.URL, error) { return u, nil }
	}
	c = &http.Client{Timeout: timeout, Transport: t}

	mu.Lock()
	if old, ok := clients[key]; ok { // 并发下先到者胜，避免重复建池
		c = old
	} else {
		clients[key] = c
	}
	mu.Unlock()
	return c, nil
}

// ValidateProxyURL 校验代理地址：scheme ∈ {http, https, socks5, socks5h} 且 host 非空。
func ValidateProxyURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy url scheme must be http/https/socks5, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("proxy url host required, e.g. http://127.0.0.1:7890")
	}
	return nil
}

// MaskProxyURL 脱敏展示用：隐藏代理地址里的密码（如 http://user:***@host:port），
// 无凭据地址原样返回。渠道勾选框向用户展示代理地址时用 —— 地址可见，凭据不泄露。
func MaskProxyURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	if u.User == nil {
		return raw
	}
	if _, hasPass := u.User.Password(); !hasPass {
		return raw
	}
	// 手动拼接（不经过 url.URL.String() —— 它会把 *** 编码成 %2A%2A%2A）
	out := u.Scheme + "://" + u.User.Username() + ":***@" + u.Host
	out += u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

// NormalizeProxyURL 宽容归一化：无 scheme 的 host:port 自动补 http:// ——
// 那是本地代理最常见的写法（127.0.0.1:7890 直接送 url.Parse 会因缺 scheme 报错）；
// socks5 等非默认类型仍需显式写 scheme。空串原样返回（= 未配置）。
func NormalizeProxyURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	return "http://" + s
}
