package relay

import (
	"slices"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// /v1/models 需与转发拦截保持一致：key 设了模型白名单就只回白名单内模型。
func TestFilterModelsByLimit(t *testing.T) {
	names := []string{"gpt-4o", "deepseek-chat", "kimi-k2"}

	// 未设限制 / key 缺失 → 原样返回
	if got := filterModelsByLimit(names, nil); !slices.Equal(got, names) {
		t.Fatalf("nil key must keep list, got %v", got)
	}
	if got := filterModelsByLimit(names, &model.ApiKey{}); !slices.Equal(got, names) {
		t.Fatalf("empty limit must keep list, got %v", got)
	}

	// 白名单命中子集（保留原顺序）
	k := &model.ApiKey{ModelLimit: "kimi-k2, gpt-4o"}
	want := []string{"gpt-4o", "kimi-k2"}
	if got := filterModelsByLimit(names, k); !slices.Equal(got, want) {
		t.Fatalf("filtered = %v, want %v", got, want)
	}

	// 全不命中 → 空列表（非 nil，避免 JSON 输出 null → 前端拿到 null 报错）
	k = &model.ApiKey{ModelLimit: "not-exist"}
	if got := filterModelsByLimit(names, k); len(got) != 0 || got == nil {
		t.Fatalf("expected empty non-nil slice, got %#v", got)
	}
}
