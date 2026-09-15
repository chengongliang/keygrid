package handlers

import (
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/op"
)

// 验证三个维度的表头与数据行组装：维度列不出现恒空列，数值列含 total_tokens / cost。
func TestBuildCSVRows(t *testing.T) {
	rows := []op.PlatformUsageAggRow{
		{Day: "2026-09-10", UserID: 7, Email: "a@x.com", Model: "gpt-5.6",
			Requests: 101, FailedRequests: 46, PromptTokens: 851724, CompletionTokens: 10159,
			AvgLatencyMs: 6039.4, Cost: 0.012345},
	}

	// day 维度：不包含 user_id/email/model 空列
	day := buildCSVRows("day", rows)
	wantHeader := "day,requests,failed_requests,prompt_tokens,completion_tokens,total_tokens,cost,avg_latency_ms"
	if got := strings.Join(day[0], ","); got != wantHeader {
		t.Fatalf("day header = %q, want %q", got, wantHeader)
	}
	wantRow := "2026-09-10,101,46,851724,10159,861883,0.012345,6039.4"
	if got := strings.Join(day[1], ","); got != wantRow {
		t.Fatalf("day row = %q, want %q", got, wantRow)
	}

	// user 维度：day/user_id/email + 数值列
	user := buildCSVRows("user", rows)
	if got := strings.Join(user[0], ","); !strings.HasPrefix(got, "day,user_id,email,") || !strings.Contains(got, ",total_tokens,") {
		t.Fatalf("user header = %q", got)
	}
	if got := strings.Join(user[1], ","); !strings.HasPrefix(got, "2026-09-10,7,a@x.com,") {
		t.Fatalf("user row = %q", got)
	}

	// model 维度：model 列开头，无 day/user 列
	model := buildCSVRows("model", rows)
	if got := strings.Join(model[0], ","); !strings.HasPrefix(got, "model,requests,") {
		t.Fatalf("model header = %q", got)
	}
	if got := strings.Join(model[1], ","); !strings.HasPrefix(got, "gpt-5.6,101,") {
		t.Fatalf("model row = %q", got)
	}
}

// 以 = + - @ 开头的单元格前插单引号，防 CSV 公式注入；其余原样。
func TestCSVSafe(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"gpt-5.6":    "gpt-5.6", // 中间的 - 不处理
		"=HYPERLINK": "'=HYPERLINK",
		"+1+1":       "'+1+1",
		"@x":         "'@x",
		"-1":         "'-1",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
