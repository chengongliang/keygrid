package upstream

import "testing"

func TestCNChatBase(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"默认", "", CNChatBaseDefault},
		{"空白", "  ", CNChatBaseDefault},
		{"官方备用域名", " https://www.workbuddy.cn/// ", "https://www.workbuddy.cn"},
		{"本地 mock", "http://127.0.0.1:9123/", "http://127.0.0.1:9123"},
		{"无协议", "www.workbuddy.cn", CNChatBaseDefault},
		{"非法协议", "file:///tmp/buddy", CNChatBaseDefault},
		{"无主机", "https:///chat", CNChatBaseDefault},
		{"错误端口", "https://www.workbuddy.cn:bad", CNChatBaseDefault},
		{"用户信息", "https://user:password@www.workbuddy.cn", CNChatBaseDefault},
		{"查询参数", "https://www.workbuddy.cn?token=secret", CNChatBaseDefault},
		{"空查询参数", "https://www.workbuddy.cn?", CNChatBaseDefault},
		{"片段", "https://www.workbuddy.cn#token", CNChatBaseDefault},
		{"非根路径", "https://www.workbuddy.cn/v2", CNChatBaseDefault},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("WB_CN_CHAT_UPSTREAM", testCase.raw)
			if got := CNChatBase(); got != testCase.want {
				t.Fatalf("CNChatBase() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestCNEndpointOverridePriority(t *testing.T) {
	t.Setenv("WB_CN_CHAT_UPSTREAM", "https://www.workbuddy.cn/")
	t.Setenv("CODEBUDDY_REFRESH_URL", "")
	path := "/v2/plugin/auth/token/refresh"
	if got := CNEndpoint(CNChatBaseDefault+path, "CODEBUDDY_REFRESH_URL"); got != "https://www.workbuddy.cn"+path {
		t.Fatalf("统一改址未生效: %q", got)
	}
	custom := "https://custom.example" + path
	if got := CNEndpoint(custom, "CODEBUDDY_REFRESH_URL"); got != custom {
		t.Fatalf("不应覆盖自定义端点: %q", got)
	}
	t.Setenv("CODEBUDDY_REFRESH_URL", CNChatBaseDefault+path)
	if got := CNEndpoint(CNChatBaseDefault+path, "CODEBUDDY_REFRESH_URL"); got != CNChatBaseDefault+path {
		t.Fatalf("显式端点应优先: %q", got)
	}
}
