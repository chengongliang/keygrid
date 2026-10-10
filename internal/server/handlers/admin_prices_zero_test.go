package handlers

import "testing"

// admin_prices_zero_test.go 一键 0 价：模型名清洗/去重 + 恒定 0 价（纯函数）。
// DB 写入路径依赖 Postgres（项目无 sqlite 测试基建），由本地 E2E 覆盖。

func TestBuildZeroPriceRows(t *testing.T) {
	ps := buildZeroPriceRows([]string{" gpt-4o ", "", "gpt-4o", "  ", "claude-3-5-sonnet", "\tgpt-4o"}, "remark-x")
	if len(ps) != 2 {
		t.Fatalf("expect 2 rows after trim/dedupe, got %d: %+v", len(ps), ps)
	}
	if ps[0].Model != "gpt-4o" || ps[1].Model != "claude-3-5-sonnet" {
		t.Fatalf("unexpected models: %+v", ps)
	}
	for _, p := range ps {
		if p.PromptPrice != 0 || p.CompletionPrice != 0 {
			t.Fatalf("price must be 0: %+v", p)
		}
		if p.Remark != "remark-x" {
			t.Fatalf("remark: %q", p.Remark)
		}
	}
}

func TestBuildZeroPriceRows_Empty(t *testing.T) {
	if ps := buildZeroPriceRows(nil, "r"); len(ps) != 0 {
		t.Fatalf("nil input should yield empty, got %+v", ps)
	}
	if ps := buildZeroPriceRows([]string{"", "  "}, "r"); len(ps) != 0 {
		t.Fatalf("blank names should be skipped, got %+v", ps)
	}
}
