package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// admin_prices_sync_test.go 定价同步：解析/名称映射/去重 + 对比分类 + 上游拉取（httptest 注入）。
// DB 写入路径依赖 Postgres（项目无 sqlite 测试基建），由本地 E2E 覆盖。

func TestParseOpenRouterPrices(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"openai/gpt-4o","pricing":{"prompt":"0.0000025","completion":"0.00001"}},
		{"id":"deepseek/deepseek-chat","pricing":{"prompt":"0.00000027","completion":"0.0000011"}},
		{"id":"vendor/free-model","pricing":{"prompt":"0","completion":"0"}},
		{"id":"vendor/negative","pricing":{"prompt":"-1","completion":"0.00001"}},
		{"id":"vendor/badstring","pricing":{"prompt":"abc","completion":"0.00001"}},
		{"id":"vendor/missing","pricing":{}},
		{"id":"openai/gpt-4o","pricing":{"prompt":"9","completion":"9"}},
		{"id":"no-slash-model","pricing":{"prompt":"0.000001","completion":"0.000002"}},
		{"id":"","pricing":{"prompt":"0.000001","completion":"0.000002"}}
	]}`)
	ps, err := parseOpenRouterPrices(body)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]orPrice{}
	for _, p := range ps {
		got[p.Model] = p
	}
	if len(ps) != 4 {
		t.Fatalf("expect 4 entries (负价/坏串/缺失/空名/重复 均跳过), got %d: %+v", len(ps), ps)
	}
	gpt := got["gpt-4o"]
	// USD/token ×1e6 → USD/1M；0.0000025 → 2.5（round12 消尾差）
	if gpt.PromptPrice != 2.5 || gpt.CompletionPrice != 10 {
		t.Fatalf("gpt-4o price: want 2.5/10, got %v/%v", gpt.PromptPrice, gpt.CompletionPrice)
	}
	if gpt.SourceID != "openai/gpt-4o" {
		t.Fatalf("source_id: %q", gpt.SourceID)
	}
	// 同名先到先得：第二条 openai/gpt-4o（9/9）不能覆盖第一条
	if got["gpt-4o"].PromptPrice == 9 {
		t.Fatal("duplicate model must keep first occurrence")
	}
	if f := got["free-model"]; f.PromptPrice != 0 || f.CompletionPrice != 0 {
		t.Fatalf("free model should be kept with 0 price: %+v", f)
	}
	if _, ok := got["no-slash-model"]; !ok {
		t.Fatal("id without slash should be used as-is")
	}
}

func TestParseOpenRouterPrices_InvalidJSON(t *testing.T) {
	if _, err := parseOpenRouterPrices([]byte("not json")); err == nil {
		t.Fatal("expect error on invalid json")
	}
}

func TestBuildSyncPreview(t *testing.T) {
	src := []orPrice{
		{Model: "new-model", PromptPrice: 1, CompletionPrice: 2, SourceID: "v/new-model"},
		{Model: "changed", PromptPrice: 3, CompletionPrice: 4, SourceID: "v/changed"},
		{Model: "same", PromptPrice: 5, CompletionPrice: 6, SourceID: "v/same"},
	}
	existing := []model.ModelPrice{
		{Model: "changed", PromptPrice: 100, CompletionPrice: 4},
		{Model: "same", PromptPrice: 5, CompletionPrice: 6},
	}
	items := buildSyncPreview(src, existing)
	got := map[string]syncPreviewItem{}
	for _, it := range items {
		got[it.Model] = it
	}
	if got["new-model"].Status != "new" || got["new-model"].CurrentPrompt != nil {
		t.Fatalf("new-model: %+v", got["new-model"])
	}
	if got["changed"].Status != "update" {
		t.Fatalf("changed: %+v", got["changed"])
	}
	if got["changed"].CurrentPrompt == nil || *got["changed"].CurrentPrompt != 100 {
		t.Fatalf("changed current price: %+v", got["changed"])
	}
	if got["same"].Status != "same" {
		t.Fatalf("same: %+v", got["same"])
	}
}

func TestFetchOpenRouterPrices_Upstream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"openai/gpt-4o","pricing":{"prompt":"0.0000025","completion":"0.00001"}}]}`)
	}))
	defer ts.Close()

	h := &AdminPricesHandler{SyncURL: ts.URL, SyncHTTPC: ts.Client()}
	ps, err := h.fetchOpenRouterPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Model != "gpt-4o" || ps[0].PromptPrice != 2.5 {
		t.Fatalf("unexpected: %+v", ps)
	}
}

func TestFetchOpenRouterPrices_UpstreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer ts.Close()

	h := &AdminPricesHandler{SyncURL: ts.URL, SyncHTTPC: ts.Client()}
	if _, err := h.fetchOpenRouterPrices(); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expect upstream status error, got %v", err)
	}
}
