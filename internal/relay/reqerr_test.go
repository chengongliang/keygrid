package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

// ---- UA 策略 ----

func TestResolveUserAgent(t *testing.T) {
	prov := func(mode, value string) *model.Provider {
		return &model.Provider{UAMode: mode, UserAgent: value}
	}
	cases := []struct {
		name     string
		p        *model.Provider
		proto    string
		clientUA string
		want     string
	}{
		{"默认透传客户端 UA", prov("", ""), protoOpenAI, "pi-agent/1.0", "pi-agent/1.0"},
		{"默认 anthropic 渠道同样透传", prov("", ""), protoAnthropic, "claude-cli/2.0", "claude-cli/2.0"},
		{"默认 codex 渠道保持固定值（返回空 = 不改写）", prov("", ""), protoResponses, "curl/8.0", ""},
		{"forward 对 codex 也透传", prov(UAModeForward, ""), protoResponses, "codex_cli_rs/9", "codex_cli_rs/9"},
		{"custom 用渠道值", prov(UAModeCustom, "MyApp/2.1"), protoOpenAI, "ignored/1", "MyApp/2.1"},
		{"custom 覆盖 codex 固定值", prov(UAModeCustom, "MyApp/2.1"), protoResponses, "", "MyApp/2.1"},
		{"custom 值为空时退回默认策略（非 codex 透传）", prov(UAModeCustom, "  "), protoOpenAI, "x/1", "x/1"},
		{"custom 值为空时退回默认策略（codex 固定）", prov(UAModeCustom, ""), protoResponses, "x/1", ""},
		{"客户端未带 UA 时不做伪装", prov("", ""), protoOpenAI, "", ""},
		{"含 CR/LF 的客户端 UA 被丢弃（防 header 注入）", prov("", ""), protoOpenAI, "evil\r\nX-Injected: 1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveUserAgent(c.p, c.proto, c.clientUA); got != c.want {
				t.Fatalf("ResolveUserAgent = %q, want %q", got, c.want)
			}
		})
	}
	// nil provider 不应 panic（防御性）
	if got := ResolveUserAgent(nil, protoOpenAI, "a/1"); got != "a/1" {
		t.Fatalf("nil provider: got %q", got)
	}
}

func TestValidUserAgent(t *testing.T) {
	if !ValidUserAgent("MyApp/1.0 (linux)") {
		t.Fatal("普通 UA 应合法")
	}
	if ValidUserAgent("") || ValidUserAgent("   ") {
		t.Fatal("空 UA 不合法")
	}
	if ValidUserAgent("a\r\nb") || ValidUserAgent("a\nb") {
		t.Fatal("含换行的 UA 不合法（header 注入）")
	}
	if ValidUserAgent(strings.Repeat("a", maxUserAgentLen+1)) {
		t.Fatal("超长 UA 不合法")
	}
}

func TestValidUAMode(t *testing.T) {
	for _, ok := range []string{"", UAModeCustom, UAModeForward} {
		if !ValidUAMode(ok) {
			t.Fatalf("%q 应合法", ok)
		}
	}
	if ValidUAMode("bogus") {
		t.Fatal("未知模式应被拒绝")
	}
}

// ---- 失败分类与摘要 ----

func TestClassifyUpstreamStatus(t *testing.T) {
	cases := map[int]string{
		400: errKindUpstream4xx,
		401: errKindUpstream4xx,
		403: errKindUpstream4xx,
		404: errKindUpstream4xx,
		422: errKindUpstream4xx,
		429: errKindUpstream429,
		500: errKindUpstream5xx,
		502: errKindUpstream5xx,
		503: errKindUpstream5xx,
		0:   errKindTransport,
	}
	for status, want := range cases {
		if got := classifyUpstreamStatus(status); got != want {
			t.Fatalf("status %d: got %q, want %q", status, got, want)
		}
	}
}

func TestSanitizeErrMessage(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		contentType string
		want        string
	}{
		{
			name: "openai 风格结构化错误",
			body: `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`,
			want: "model not found (type=invalid_request_error, code=model_not_found)",
		},
		{
			name: "anthropic 风格",
			body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want: "Overloaded (type=overloaded_error)",
		},
		{
			name: "数字 code",
			body: `{"error":{"message":"rate limited","code":429}}`,
			want: "rate limited (code=429)",
		},
		{
			name: "只有 status 字段",
			body: `{"error":{"status":"RESOURCE_EXHAUSTED"}}`,
			want: "RESOURCE_EXHAUSTED",
		},
		{
			name: "换行压成空格",
			body: `{"error":{"message":"line1\nline2\t "}}`,
			want: "line1 line2",
		},
		{
			name:        "非 JSON 只记形状不记内容",
			body:        `<html><body>502 Bad Gateway from SECRET-EDGE-HOST</body></html>`,
			contentType: "text/html; charset=utf-8",
			want:        "non-JSON upstream body (text/html, 63 bytes)",
		},
		{
			name: "解析不出错误字段时不留正文（可能含请求回显）",
			body: `{"choices":[{"message":{"content":"PROMPT ECHO"}}]}`,
			want: "",
		},
		{name: "空体", body: "   ", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeErrMessage(c.body, c.contentType); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSanitizeErrMessageTruncates(t *testing.T) {
	long := strings.Repeat("x", 4000)
	got := sanitizeErrMessage(`{"error":{"message":"`+long+`"}}`, "application/json")
	if n := len([]rune(got)); n != maxErrMessageLen+1 { // +1 = 省略号
		t.Fatalf("截断后长度 %d, want %d", n, maxErrMessageLen+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("截断应有省略号")
	}
}

// ---- 异步写入 + 采样 ----

type fakeErrStore struct {
	rows []*model.RequestError
}

func (s *fakeErrStore) InsertRequestErrors(rows []*model.RequestError) error {
	s.rows = append(s.rows, rows...)
	return nil
}

func TestErrorWriterFlushAndSampling(t *testing.T) {
	store := &fakeErrStore{}
	ew := NewErrorWriter(store, 1000, 10*time.Millisecond)
	defer ew.Stop()

	// 同一渠道超过每分钟上限的部分被丢弃
	for i := 0; i < maxErrorsPerProviderPerMinute+25; i++ {
		ew.Record(&model.RequestError{ProviderID: 7, Kind: errKindUpstream5xx})
	}
	// 另一个渠道独立计数
	for i := 0; i < 3; i++ {
		ew.Record(&model.RequestError{ProviderID: 8, Kind: errKindTransport})
	}
	ew.Stop()

	if len(store.rows) != maxErrorsPerProviderPerMinute+3 {
		t.Fatalf("落库 %d 条, want %d", len(store.rows), maxErrorsPerProviderPerMinute+3)
	}
	byProvider := map[int64]int{}
	for _, r := range store.rows {
		byProvider[r.ProviderID]++
	}
	if byProvider[7] != maxErrorsPerProviderPerMinute || byProvider[8] != 3 {
		t.Fatalf("按渠道计数错误: %+v", byProvider)
	}
}

func TestErrorWriterNilSafety(t *testing.T) {
	var ew *ErrorWriter
	ew.Record(&model.RequestError{}) // 不应 panic
	ew.Stop()
}
