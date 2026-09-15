package handlers

import (
	"net/http"
	"testing"
)

// parseUsageFilter 解析 ?key_id=&provider_id=&model= 筛选参数（缺省 = 不过滤）。
// by-provider 维度新增后，provider_id 需与 key_id 同样严格校验（非法值整体拒绝，不静默降级）。
func TestParseUsageFilterProviderID(t *testing.T) {
	mk := func(q string) *http.Request {
		r, err := http.NewRequest("GET", "/api/usage?"+q, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// 缺省：全部不过滤
	f, err := parseUsageFilter(mk(""))
	if err != nil {
		t.Fatalf("empty query: %v", err)
	}
	if f.ApiKeyID != 0 || f.ProviderID != 0 || f.Model != "" {
		t.Fatalf("empty query filter = %+v, want zeros", f)
	}

	// provider_id + key_id + model 全量解析
	f, err = parseUsageFilter(mk("key_id=3&provider_id=7&model=gpt-5"))
	if err != nil {
		t.Fatalf("valid query: %v", err)
	}
	if f.ApiKeyID != 3 || f.ProviderID != 7 || f.Model != "gpt-5" {
		t.Fatalf("filter = %+v, want key=3 provider=7 model=gpt-5", f)
	}

	// 非法 provider_id：拒绝
	for _, bad := range []string{"provider_id=abc", "provider_id=-1", "provider_id=0"} {
		if _, err = parseUsageFilter(mk(bad)); err == nil {
			t.Fatalf("provider_id=%q should be rejected", bad)
		}
	}
}
