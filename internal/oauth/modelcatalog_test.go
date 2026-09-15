package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- 目录解析校验 ----

func TestParseCodexModelCatalog(t *testing.T) {
	// 合法：字符串数组形状（KeyGrid 精简快照格式）
	models, err := parseCodexModelCatalog([]byte(`{"comment":"x","models":["gpt-6-astra","gpt-5.5"]}`))
	if err != nil || len(models) != 2 || models[0] != "gpt-6-astra" {
		t.Fatalf("string shape: models=%v err=%v", models, err)
	}

	// 合法：router-for-me 原始对象形状（slug 字段）
	models, err = parseCodexModelCatalog([]byte(`{"models":[{"slug":"gpt-6-astra","x":1},{"slug":"gpt-5.5"}]}`))
	if err != nil || len(models) != 2 || models[1] != "gpt-5.5" {
		t.Fatalf("object shape: models=%v err=%v", models, err)
	}

	// 去重
	models, err = parseCodexModelCatalog([]byte(`{"models":["a","a","b"]}`))
	if err != nil || len(models) != 2 {
		t.Fatalf("dedup: models=%v err=%v", models, err)
	}

	// 非法：缺 slug / 空 models / 坏 JSON / 空数据
	for _, bad := range []string{
		`{"models":[{"name":"x"}]}`,
		`{"models":[]}`,
		`{"models":["ok", 123]}`,
		`not json`,
		``,
	} {
		if _, err := parseCodexModelCatalog([]byte(bad)); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

// TestEmbeddedCodexSnapshotValid 内嵌兜底快照必须永远可解析（防止入库坏数据）。
func TestEmbeddedCodexSnapshotValid(t *testing.T) {
	models, err := parseCodexModelCatalog(embeddedCodexModelsJSON)
	if err != nil {
		t.Fatalf("embedded snapshot invalid: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("embedded snapshot empty")
	}
	// 关键模型必须在内嵌快照里（远程刷新前的最后防线）
	var hasAstra bool
	for _, m := range models {
		if m == "gpt-6-astra" {
			hasAstra = true
		}
	}
	if !hasAstra {
		t.Fatalf("embedded snapshot missing gpt-6-astra: %v", models)
	}
}

// ---- 目录缓存与刷新 ----

func setStoreForTest(t *testing.T, models []string) {
	t.Helper()
	codexModelCatalogStore.mu.Lock()
	defer codexModelCatalogStore.mu.Unlock()
	codexModelCatalogStore.models = models
}

func TestCodexModelCatalogProviderGate(t *testing.T) {
	defer setStoreForTest(t, nil)
	setStoreForTest(t, []string{"gpt-6-astra"})

	if got := CodexModelCatalog("openai"); len(got) != 1 {
		t.Fatalf("openai catalog = %v", got)
	}
	if got := CodexModelCatalog("kimi"); got != nil {
		t.Fatalf("non-openai should be nil, got %v", got)
	}
	// 返回副本：外部修改不影响缓存
	got := CodexModelCatalog("openai")
	got[0] = "tampered"
	if CodexModelCatalog("openai")[0] != "gpt-6-astra" {
		t.Fatal("catalog must return a copy")
	}
}

func TestRefreshCodexModelCatalog(t *testing.T) {
	defer setStoreForTest(t, nil)
	ctx := context.Background()

	// 第一源失败 → 容灾到第二源
	var firstHits int
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":["gpt-remote-1","gpt-remote-2"]}`))
	}))
	defer srv2.Close()

	setStoreForTest(t, []string{"gpt-old"})
	if ok := refreshCodexModelCatalog(ctx, srv1.Client(), []string{srv1.URL, srv2.URL}); !ok {
		t.Fatal("expected success via second source")
	}
	if firstHits != 1 {
		t.Fatalf("first source should be attempted once, got %d", firstHits)
	}
	if got := CodexModelCatalog("openai"); len(got) != 2 || got[0] != "gpt-remote-1" {
		t.Fatalf("catalog after failover: %v", got)
	}

	// 内容无变化 → 缓存不重设（setCodexModelCatalog 返回 false）
	if ok := refreshCodexModelCatalog(ctx, srv2.Client(), []string{srv2.URL}); !ok {
		t.Fatal("refresh should report success")
	}
	if got := CodexModelCatalog("openai"); len(got) != 2 {
		t.Fatalf("catalog should stay: %v", got)
	}

	// 远程内容变化 → 更新
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"slug":"gpt-new"}]}`))
	}))
	defer srv3.Close()
	if ok := refreshCodexModelCatalog(ctx, srv3.Client(), []string{srv3.URL}); !ok {
		t.Fatal("expected update")
	}
	if got := CodexModelCatalog("openai"); len(got) != 1 || got[0] != "gpt-new" {
		t.Fatalf("catalog after change: %v", got)
	}

	// 全部失败 → 保留当前数据
	setStoreForTest(t, []string{"gpt-keep"})
	if ok := refreshCodexModelCatalog(ctx, srv1.Client(), []string{srv1.URL}); ok {
		t.Fatal("all sources down should fail")
	}
	if got := CodexModelCatalog("openai"); len(got) != 1 || got[0] != "gpt-keep" {
		t.Fatalf("catalog must be kept on failure: %v", got)
	}
}

func TestRefreshRejectsInvalidCatalog(t *testing.T) {
	defer setStoreForTest(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"no_slug":true}]}`))
	}))
	defer srv.Close()

	setStoreForTest(t, []string{"gpt-keep"})
	if ok := refreshCodexModelCatalog(context.Background(), srv.Client(), []string{srv.URL}); ok {
		t.Fatal("invalid catalog must be rejected")
	}
	if got := CodexModelCatalog("openai"); len(got) != 1 || got[0] != "gpt-keep" {
		t.Fatalf("catalog must be kept: %v", got)
	}
}

func TestRefreshOversizedBodyRejected(t *testing.T) {
	defer setStoreForTest(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":["` + strings.Repeat("x", codexModelsMaxSize+10) + `"]}`))
	}))
	defer srv.Close()

	if ok := refreshCodexModelCatalog(context.Background(), srv.Client(), []string{srv.URL}); ok {
		t.Fatal("oversized catalog must be rejected")
	}
	if CodexModelCatalog("openai") != nil {
		t.Fatal("store should remain untouched")
	}
}
