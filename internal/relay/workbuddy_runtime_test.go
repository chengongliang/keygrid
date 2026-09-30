package relay

import (
	"net/http"
	"testing"
	"time"
)

// workbuddy_runtime_test.go 上游错误 → 运行态事件 的抽取逻辑。
//
// Admit/Observe 的主循环接线依赖真实 DB，这里只测可纯函数化的部分：
// Retry-After 解析与业务错误码提取（6004 = 上游限额，决定是否设模型冷却）。

func TestParseRetryAfter(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"空值", "", 0},
		{"纯空格", "   ", 0},
		{"秒数", "60", 60 * time.Second},
		{"带空格秒数", " 30 ", 30 * time.Second},
		{"零秒", "0", 0},
		{"负数", "-5", 0},
		{"HTTP 日期", now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 0}, // 允许解析误差：只断言 > 0
		{"过去日期", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
		{"垃圾值", "soon", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRetryAfter(c.in)
			if c.name == "HTTP 日期" {
				if got <= 0 || got > 2*time.Minute {
					t.Fatalf("HTTP 日期应解析出 0<d<=2m, got %v", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestUpstreamCodeFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"非上游错误", errTest("boom"), 0},
		{"无正文", &UpstreamError{Status: 429}, 0},
		{"顶层 code", &UpstreamError{Status: 200, Body: `{"code":6004,"msg":"limit"}`}, 6004},
		{"error.code", &UpstreamError{Status: 200, Body: `{"error":{"code":12302,"message":"x"}}`}, 12302},
		{"status 兜底", &UpstreamError{Status: 200, Body: `{"status":11128}`}, 11128},
		{"字符串数字", &UpstreamError{Status: 200, Body: `{"code":"6004"}`}, 6004},
		{"非数字 code", &UpstreamError{Status: 200, Body: `{"code":"E_LIMIT"}`}, 0},
		{"非 JSON 正文", &UpstreamError{Status: 502, Body: "<html>bad gateway</html>"}, 0},
		{"无 code 字段", &UpstreamError{Status: 400, Body: `{"message":"bad request"}`}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := upstreamCodeFromError(c.err); got != c.want {
				t.Fatalf("upstreamCodeFromError = %d, want %d", got, c.want)
			}
		})
	}
}

func TestRetryAfterFrom(t *testing.T) {
	if d := retryAfterFrom(errTest("x")); d != 0 {
		t.Fatalf("非上游错误应为 0, got %v", d)
	}
	if d := retryAfterFrom(&UpstreamError{Status: 429, RetryAfter: 12 * time.Second}); d != 12*time.Second {
		t.Fatalf("应透传 RetryAfter, got %v", d)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
