package middleware

import "testing"

func TestIPAllowed(t *testing.T) {
	cases := []struct {
		whitelist string
		ip        string
		want      bool
	}{
		{"", "1.2.3.4", true},                        // 空白名单 = 不限
		{"1.2.3.4", "1.2.3.4", true},                 // 精确命中
		{"1.2.3.4", "1.2.3.5", false},                // 精确不命中
		{"1.2.3.4,10.0.0.0/8", "10.1.2.3", true},     // CIDR 命中
		{"1.2.3.4,10.0.0.0/8", "11.1.2.3", false},    // 都不命中
		{" 1.2.3.4 , 10.0.0.0/8 ", "10.1.2.3", true}, // 带空格
		{"2001:db8::/32", "2001:db8::1", true},       // IPv6 CIDR
		{"2001:db8::/32", "2001:db9::1", false},      // IPv6 不命中
		{"::ffff:1.2.3.4", "1.2.3.4", true},          // v4-mapped v6 写法等价
		{"not-an-ip", "1.2.3.4", false},              // 非法条目不命中
		{"1.2.3.4", "", false},                       // 无法解析的客户端 IP
	}
	for _, c := range cases {
		if got := IPAllowed(c.whitelist, c.ip); got != c.want {
			t.Errorf("IPAllowed(%q, %q) = %v, want %v", c.whitelist, c.ip, got, c.want)
		}
	}
}

func TestModelAllowed(t *testing.T) {
	cases := []struct {
		limit string
		model string
		want  bool
	}{
		{"", "gpt-4o", true},                                // 空 = 不限
		{"gpt-4o,deepseek-chat", "deepseek-chat", true},     // 命中
		{"gpt-4o,deepseek-chat", "claude-3", false},         // 不命中
		{" gpt-4o , deepseek-chat ", "deepseek-chat", true}, // 白名单带空格可容忍
		{"gpt-4o", "", false},                               // 空模型不命中
	}
	for _, c := range cases {
		if got := ModelAllowed(c.limit, c.model); got != c.want {
			t.Errorf("ModelAllowed(%q, %q) = %v, want %v", c.limit, c.model, got, c.want)
		}
	}
}
