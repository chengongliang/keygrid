package ssrf

import (
	"net"
	"testing"
)

func TestHostAllowed(t *testing.T) {
	tests := []struct {
		name string
		env  string
		host string
		want bool
	}{
		{"空环境变量", "", "new-api.corp.lan", false},
		{"精确匹配", "new-api.corp.lan", "new-api.corp.lan", true},
		{"精确匹配大小写归一", "New-API.Corp.LAN", "new-api.corp.lan", true},
		{"精确不匹配子域", "new-api.corp.lan", "other.corp.lan", false},
		{"后缀通配点形式", ".corp.lan", "new-api.corp.lan", true},
		{"后缀通配星号形式", "*.corp.lan", "new-api.corp.lan", true},
		{"后缀通配多级子域", ".corp.lan", "a.b.corp.lan", true},
		{"后缀通配不含裸域", ".corp.lan", "corp.lan", false},
		{"后缀不匹配其他域", ".corp.lan", "evi-lcorp.lan", false},
		{"FQDN 尾点归一化", "new-api.corp.lan", "new-api.corp.lan.", true},
		{"多值逗号分隔", "a.lan, .corp.lan, b.lan", "x.corp.lan", true},
		{"空 host", ".corp.lan", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SSRF_ALLOWED_HOSTS", tt.env)
			if got := hostAllowed(tt.host); got != tt.want {
				t.Errorf("hostAllowed(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestIPAllowedCIDRs(t *testing.T) {
	t.Setenv("SSRF_ALLOWED_CIDRS", "10.20.0.0/16, 192.168.1.0/24")
	tests := []struct {
		ip   string
		want bool
	}{
		{"10.20.172.25", true},
		{"10.80.1.1", false}, // 白名单外私网
		{"192.168.1.10", true},
		{"192.168.2.10", false},
		{"8.8.8.8", false},
	}
	for _, tt := range tests {
		if got := ipAllowed(net.ParseIP(tt.ip)); got != tt.want {
			t.Errorf("ipAllowed(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

// TestIPAllowedV4Mapped CIDR 白名单对 v4-mapped IPv6 也生效。
func TestIPAllowedV4Mapped(t *testing.T) {
	t.Setenv("SSRF_ALLOWED_CIDRS", "10.20.0.0/16")
	ip := net.ParseIP("::ffff:10.20.172.25")
	if !ipAllowed(ip) {
		t.Errorf("ipAllowed(v4-mapped %v) = false, want true", ip)
	}
}

func TestCheckHostWhitelist(t *testing.T) {
	// localhost 解析到 127.0.0.1/::1，默认被拦截
	if err := CheckHost("localhost"); err == nil {
		t.Fatal("CheckHost(localhost) err = nil, want private/reserved error")
	}

	// 主机名白名单放行
	t.Setenv("SSRF_ALLOWED_HOSTS", "localhost")
	if err := CheckHost("localhost"); err != nil {
		t.Fatalf("CheckHost(localhost) with whitelist = %v, want nil", err)
	}

	// 白名单外私网仍拦截
	if err := CheckHost("127.0.0.2"); err == nil {
		t.Fatal("CheckHost(127.0.0.2) err = nil, want blocked")
	}

	// CIDR 白名单放行直填的内网 IP
	t.Setenv("SSRF_ALLOWED_CIDRS", "127.0.0.0/8")
	if err := CheckHost("127.0.0.2"); err != nil {
		t.Fatalf("CheckHost(127.0.0.2) with CIDR whitelist = %v, want nil", err)
	}
}

func TestCheckBaseURLWhitelist(t *testing.T) {
	t.Setenv("SSRF_ALLOWED_HOSTS", "localhost")

	if err := CheckBaseURL("http://localhost:3000/v1"); err != nil {
		t.Fatalf("CheckBaseURL(localhost) = %v, want nil", err)
	}
	// 白名单按 host 生效，端口/路径不影响
	if err := CheckBaseURL("https://localhost/api"); err != nil {
		t.Fatalf("CheckBaseURL(https://localhost) = %v, want nil", err)
	}
	// 非 http(s) scheme 仍拒绝
	if err := CheckBaseURL("file:///etc/passwd"); err == nil {
		t.Fatal("CheckBaseURL(file://) err = nil, want scheme error")
	}
}
