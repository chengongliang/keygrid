package ssrf

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ssrf.go SSRF 防护：provider base_url 拒绝私网/环回/链路本地上网地址。
// 上游 HTTP client 通过受控 DialContext 强制走防护，绕过 host 校验也连不出去。
//
// 自托管场景放行内网上游（三档，按需选择）：
//   1. SSRF_ALLOW_INTERNAL=1                全部放行（自托管推荐；多租户/不可信用户可添加供应商时慎用）
//   2. SSRF_ALLOWED_HOSTS=host1,.corp.lan   主机名白名单（逗号分隔，.x.com/*.x.com 为后缀通配）
//   3. SSRF_ALLOWED_CIDRS=10.0.0.0/8        IP/CIDR 白名单（base_url 直填 IP）
// 白名单只豁免对应主机/IP，其余私网地址仍被拦截。

// allowedHosts 解析 SSRF_ALLOWED_HOSTS。每次调用读取，测试可随时改环境变量。
func allowedHosts() []string {
	raw := os.Getenv("SSRF_ALLOWED_HOSTS")
	if raw == "" {
		return nil
	}
	var hosts []string
	for _, h := range strings.Split(raw, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimSuffix(h, ".") // FQDN 尾点归一化
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// hostAllowed host 是否在 SSRF_ALLOWED_HOSTS 白名单内。
// 支持：精确匹配；".corp.lan" / "*.corp.lan" 后缀通配（匹配任意层级子域，不含裸域）。
func hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	for _, h := range allowedHosts() {
		if h == host {
			return true
		}
		suffix, ok := strings.CutPrefix(h, "*")
		if !ok {
			suffix = h
		}
		if strings.HasPrefix(suffix, ".") && strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// allowedCIDRs 解析 SSRF_ALLOWED_CIDRS，无法解析的条目忽略。
func allowedCIDRs() []*net.IPNet {
	raw := os.Getenv("SSRF_ALLOWED_CIDRS")
	if raw == "" {
		return nil
	}
	var nets []*net.IPNet
	for _, s := range strings.Split(raw, ",") {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(s)); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

// ipAllowed ip 是否命中 CIDR 白名单（v4-mapped 先还原再比对）。
func ipAllowed(ip net.IP) bool {
	cidrs := allowedCIDRs()
	if len(cidrs) == 0 {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// CheckHost 解析 hostname 并拒绝私网/环回/未指定地址。
// 返回 nil 表示公网地址，允许通过。
// SSRF_ALLOWED_HOSTS 命中时放行（管理员显式声明的内网上游）。
// SSRF_ALLOW_INTERNAL=1 时全部放行（自托管可选；多租户/不可信用户场景慎用）。
func CheckHost(host string) error {
	if os.Getenv("SSRF_ALLOW_INTERNAL") == "1" || hostAllowed(host) {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isBlocked(ip) {
			return fmt.Errorf("host %s resolves to private/reserved address", host)
		}
	}
	return nil
}

func isBlocked(ip net.IP) bool {
	if ipAllowed(ip) { // CIDR 白名单优先于所有保留段
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// IPv4-mapped IPv6 绕过检测：还原成 v4 再判
	if v4 := ip.To4(); v4 != nil {
		return isBlockedV4(v4)
	}
	return false
}

// isBlockedV4 补充 GORM 未覆盖的保留段（0.0.0.0/8、100.64/10 CGN、198.18/15）。
func isBlockedV4(ip net.IP) bool {
	blocks := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
	}
	for _, cidr := range blocks {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// SafeTransport 构建 SSRF 防护的 http.Transport：DialContext 每次连接前重新解析并校验 IP，
// 防 DNS rebinding（校验时公网、连接时被 rebinding 到私网）。
func SafeTransport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if err := CheckHost(host); err != nil {
				return nil, err
			}
			d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
			return d.DialContext(ctx, network, addr) // port 已含在 addr
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
}

// CheckBaseURL 创建/更新渠道时的入口校验：解析 URL → host 拒私网。
// 返回 nil 表示通过。
func CheckBaseURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url scheme must be http/https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("base_url host required")
	}
	return CheckHost(host)
}
