package httpx

import (
	"net/http"
	"testing"
	"time"
)

func TestValidateProxyURL(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:7890",
		"https://proxy.corp.lan:8443",
		"socks5://127.0.0.1:1080",
		"socks5h://user:pass@10.0.0.1:1080",
	}
	for _, u := range valid {
		if err := ValidateProxyURL(u); err != nil {
			t.Errorf("ValidateProxyURL(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"",               // 空串：设置页用空表示未配置，校验层拒绝（调用方先判空）
		"127.0.0.1:7890", // 缺 scheme
		"ftp://x:1",      // 非法 scheme
		"http://",        // 缺 host
		"://x",           // 解析失败
	}
	for _, u := range invalid {
		if err := ValidateProxyURL(u); err == nil {
			t.Errorf("ValidateProxyURL(%q) = nil, want error", u)
		}
	}
}

// MaskProxyURL：展示用脱敏 —— 隐藏密码，无凭据地址原样。
func TestMaskProxyURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://proxy.example.com:3198", "http://proxy.example.com:3198"}, // 无凭据原样
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"http://user:secret@10.0.0.1:8080", "http://user:***@10.0.0.1:8080"}, // 密码打码
		{"not a url", "not a url"},                                            // 解析失败原样返回，不 panic
	}
	for _, c := range cases {
		if got := MaskProxyURL(c.in); got != c.want {
			t.Errorf("MaskProxyURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// NormalizeProxyURL：无 scheme 的 host:port 自动补 http://（本地代理最常见写法）。
func TestNormalizeProxyURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"  localhost:7890 ", "http://localhost:7890"},
		{"user:pass@10.0.0.1:8080", "http://user:pass@10.0.0.1:8080"},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"}, // 已带 scheme 原样
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeProxyURL(c.in); got != c.want {
			t.Errorf("NormalizeProxyURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 归一化产物必须过校验
	if err := ValidateProxyURL(NormalizeProxyURL("127.0.0.1:7890")); err != nil {
		t.Errorf("normalized url must validate: %v", err)
	}
}

// Client 代理语义：proxyURL 固定写进 Transport.Proxy；空 = ProxyFromEnvironment。
func TestClientProxyTransport(t *testing.T) {
	probeReq, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)

	c, err := Client(time.Second, "socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	u, e := tr.Proxy(probeReq)
	if e != nil || u == nil || u.Scheme != "socks5" || u.Host != "127.0.0.1:1080" {
		t.Fatalf("proxy = %v, %v", u, e)
	}

	direct, err := Client(time.Second, "")
	if err != nil {
		t.Fatalf("Client direct: %v", err)
	}
	dtr := direct.Transport.(*http.Transport)
	if dtr.Proxy == nil {
		t.Fatal("direct client must keep ProxyFromEnvironment")
	}
	if u2, _ := dtr.Proxy(probeReq); u2 != nil {
		t.Fatalf("direct client must not pin a proxy, got %v", u2)
	}
}

// Client 共享：同 (timeout, proxy) 复用实例；不同参数各自独立。
func TestClientShare(t *testing.T) {
	a1, _ := Client(time.Second, "http://127.0.0.1:7890")
	a2, _ := Client(time.Second, "http://127.0.0.1:7890")
	if a1 != a2 {
		t.Fatal("same key must return shared client")
	}
	b1, _ := Client(2*time.Second, "http://127.0.0.1:7890")
	if a1 == b1 {
		t.Fatal("different timeout must not share")
	}
	c1, _ := Client(time.Second, "http://127.0.0.1:7891")
	if a1 == c1 {
		t.Fatal("different proxy must not share")
	}
	if _, err := Client(time.Second, "not-a-proxy"); err == nil {
		t.Fatal("invalid proxy must be rejected")
	}
}
