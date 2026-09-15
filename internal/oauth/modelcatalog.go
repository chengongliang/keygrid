package oauth

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/httpx"
)

// modelcatalog.go Codex 上游模型目录（远程自动刷新 + 内嵌兜底）。
//
// Codex 上游（chatgpt.com/backend-api/codex/responses）没有 /v1/models 端点，
// 平台拉不到真实模型列表。对齐 CLIProxyAPI（router-for-me）的做法：
//   - 编译期内嵌一份精简快照 models/codex_client_models.json 作离线兜底；
//   - 启动时立即从远程目录仓库拉取一次，之后每 3 小时刷新；
//   - 拉取失败 / 校验失败保留当前数据（兜底永远可用）。
//
// 远程目录由 router-for-me/models 项目维护（从 Codex 客户端整理发布），
// 上游出新模型时无需发版 KeyGrid，最多 3 小时后自动出现。
// 目录 URL 为代码内固定常量（非用户输入），不涉 SSRF 面。

//go:embed models/codex_client_models.json
var embeddedCodexModelsJSON []byte

// codexModelCatalogURLs 远程模型目录源（顺序容灾）。
var codexModelCatalogURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json",
	"https://models.router-for.me/codex_client_models.json",
}

const (
	codexModelsRefreshInterval = 3 * time.Hour
	codexModelsFetchTimeout    = 15 * time.Second
	codexModelsMaxSize         = 8 << 20 // 目录含元数据可能较大，上限对齐 CLIProxyAPI 8MB
)

// codexModelStore 目录缓存（规范化为 slug 列表）。
type codexModelStore struct {
	mu     sync.RWMutex
	models []string
}

var codexModelCatalogStore = &codexModelStore{}

func init() {
	models, err := parseCodexModelCatalog(embeddedCodexModelsJSON)
	if err != nil {
		log.Printf("[modelcatalog] embedded codex_client_models.json invalid: %v", err)
		return
	}
	codexModelCatalogStore.models = models
}

// CodexModelCatalog 返回指定 oauth provider 的 Codex 模型目录快照：
// 远程缓存（含启动刷新成功后的数据）优先，内嵌兜底次之。
// 非 openai provider 或两者皆空时返回 nil（调用方回退 preset.Models）。
func CodexModelCatalog(providerKey string) []string {
	if providerKey != "openai" {
		return nil
	}
	codexModelCatalogStore.mu.RLock()
	defer codexModelCatalogStore.mu.RUnlock()
	if len(codexModelCatalogStore.models) == 0 {
		return nil
	}
	return append([]string(nil), codexModelCatalogStore.models...)
}

// parseCodexModelCatalog 解析并校验目录 JSON（远程与内嵌共用）：
// {"comment": "...", "models": ["gpt-6-astra", ...]}，slug 必须非空字符串。
// 另兼容 router-for-me 原始对象形状 {"models":[{"slug":"..."}]}。
func parseCodexModelCatalog(data []byte) ([]string, error) {
	if len(data) == 0 || len(data) > codexModelsMaxSize {
		return nil, fmt.Errorf("catalog size %d out of range", len(data))
	}
	var parsed struct {
		Models []any `json:"models"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	if len(parsed.Models) == 0 {
		return nil, fmt.Errorf("catalog has no models")
	}
	seen := make(map[string]struct{}, len(parsed.Models))
	models := make([]string, 0, len(parsed.Models))
	for _, mv := range parsed.Models {
		var slug string
		switch v := mv.(type) {
		case string:
			slug = v
		case map[string]any:
			slug, _ = v["slug"].(string)
		}
		if slug == "" {
			return nil, fmt.Errorf("catalog entry without slug")
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		models = append(models, slug)
	}
	return models, nil
}

// refreshCodexModelCatalog 依次尝试所有远程源，第一个成功且校验通过的目录生效。
// 内容无变化不换缓存；全部失败返回 false 并保留当前数据。
func refreshCodexModelCatalog(ctx context.Context, client *http.Client, urls []string) bool {
	for _, u := range urls {
		fetchCtx, cancel := context.WithTimeout(ctx, codexModelsFetchTimeout)
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, codexModelsMaxSize+1))
		resp.Body.Close()
		cancel()
		if err != nil || len(data) > codexModelsMaxSize {
			continue
		}
		models, err := parseCodexModelCatalog(data)
		if err != nil {
			log.Printf("[modelcatalog] catalog from %s rejected: %v", u, err)
			continue
		}
		if setCodexModelCatalog(models) {
			log.Printf("[modelcatalog] codex catalog updated from %s (%d models)", u, len(models))
		} else {
			log.Printf("[modelcatalog] codex catalog fetched from %s, no change (%d models)", u, len(models))
		}
		return true
	}
	return false
}

// setCodexModelCatalog 原子更新缓存；内容无变化返回 false。
func setCodexModelCatalog(models []string) bool {
	codexModelCatalogStore.mu.Lock()
	defer codexModelCatalogStore.mu.Unlock()
	if slicesEqual(codexModelCatalogStore.models, models) {
		return false
	}
	codexModelCatalogStore.models = models
	return true
}

// slicesEqual 轻量等值比较（避免引入 slices 包约束 go 版本语义）。
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// StartCodexModelCatalogUpdater 启动目录刷新后台任务：启动立即刷新一次，
// 之后每 3 小时一轮。sync.Once 保证全局只启动一个。
func StartCodexModelCatalogUpdater(ctx context.Context) {
	var once sync.Once
	go once.Do(func() {
		client, err := httpx.Client(codexModelsFetchTimeout, "")
		if err != nil {
			log.Printf("[modelcatalog] http client: %v (catalog stays on embedded snapshot)", err)
			return
		}
		if !refreshCodexModelCatalog(ctx, client, codexModelCatalogURLs) {
			log.Printf("[modelcatalog] initial refresh failed, keeping embedded snapshot")
		}
		ticker := time.NewTicker(codexModelsRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !refreshCodexModelCatalog(ctx, client, codexModelCatalogURLs) {
					log.Printf("[modelcatalog] periodic refresh failed, keeping current catalog")
				}
			}
		}
	})
}
