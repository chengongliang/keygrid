package model

import (
	"slices"
	"testing"
)

// ProviderIDs 是 key 级渠道白名单的解析入口：空 = 不限；非法/重复项忽略。
func TestProviderIDs(t *testing.T) {
	cases := []struct {
		limit string
		want  []int64
	}{
		{"", nil},
		{"   ", nil},
		{"3", []int64{3}},
		{"3,5,7", []int64{3, 5, 7}},
		{" 3 , 5 ", []int64{3, 5}},
		{"3,3,5", []int64{3, 5}}, // 去重
		{"abc,5", []int64{5}},    // 非法项忽略
		{"-1,0,5", []int64{5}},   // 非正数忽略
		{"1e3,4", []int64{4}},    // 科学计数不是合法 ID
	}
	for _, c := range cases {
		k := &ApiKey{ProviderLimit: c.limit}
		if got := k.ProviderIDs(); !slices.Equal(got, c.want) {
			t.Errorf("ProviderIDs(%q) = %v, want %v", c.limit, got, c.want)
		}
	}

	// nil 接收者安全（防御：ctx 里没有 key 对象时调用）
	var nilKey *ApiKey
	if got := nilKey.ProviderIDs(); got != nil {
		t.Errorf("nil key must return nil, got %v", got)
	}
}
