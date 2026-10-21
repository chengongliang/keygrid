package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// models.go Qoder 模型目录：GET {chatBase}/algo/api/v2/model/list（COSY 签名）。
//
// 这个接口不只是模型名列表 —— 每个模型条目本身是 chat 请求必须回传的
// model_config（少传或传错，上游会静默降级到别的模型）。所以目录在进程内
// 缓存 1 小时：既给「拉取模型」用，也给转发路径取 model_config。
//
// 目录里 enable:false 的模型（账号策略在 IDE 里隐藏，但上游仍可路由）会标记
// Hidden，转发路径照样能用。

// catalogTTL 目录缓存有效期（与 9router 的 1h 一致）。
const catalogTTL = time.Hour

// catalogFetchTimeout 目录单次请求超时。
const catalogFetchTimeout = 15 * time.Second

// Model 一个可路由的 Qoder 模型。
type Model struct {
	Key             string // 上游模型 key（auto / ultimate / qmodel_latest …）
	Name            string // 展示名
	ContextLength   int
	IsVL            bool
	IsReasoning     bool
	MaxOutputTokens int
	Description     string
	Hidden          bool // enable:false：IDE 不展示，但上游仍接受
}

// Catalog 一次目录拉取的结果。
type Catalog struct {
	Models    []Model
	Configs   map[string]json.RawMessage // key → 原始 model_config
	FetchedAt time.Time
}

// ModelKeys 目录里的全部模型 key（供渠道预设 / 校验）。
func (c *Catalog) ModelKeys() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.Key)
	}
	return out
}

// Config 取某个模型的原始 model_config。
func (c *Catalog) Config(key string) (json.RawMessage, bool) {
	if c == nil || c.Configs == nil {
		return nil, false
	}
	cfg, ok := c.Configs[key]
	return cfg, ok
}

// catalogEntry 上游 chat[] 条目（字段按需取，json.RawMessage 保留原始配置）。
type catalogEntry struct {
	Key             string      `json:"key"`
	DisplayName     string      `json:"display_name"`
	Enable          *bool       `json:"enable"`
	MaxInputTokens  json.Number `json:"max_input_tokens"`
	MaxOutputTokens json.Number `json:"max_output_tokens"`
	IsVL            bool        `json:"is_vl"`
	IsReasoning     bool        `json:"is_reasoning"`
	Description     string      `json:"description"`
}

// catalogResponse 上游响应体：{"chat":[...]}。
type catalogResponse struct {
	Chat []json.RawMessage `json:"chat"`
}

// cachedCatalog 缓存条目。
type cachedCatalog struct {
	expiresAt time.Time
	catalog   *Catalog
}

// catalogFlight 同凭据并发请求合并。
type catalogFlight struct {
	done    chan struct{}
	catalog *Catalog
	err     error
}

// fetchCatalog 拉取目录（不缓存）。
func fetchCatalog(ctx context.Context, client Doer, rc Resolved) (*Catalog, error) {
	target := ModelListURL(rc.Region, rc.Token)
	ctx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	hdr, err := BuildHeaders(nil, target, rc.Creds())
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qoder model list: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("qoder model list: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: truncate(string(body), 512)}
	}

	var parsed catalogResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("qoder model list: parse response: %w", err)
	}
	cat := &Catalog{Configs: map[string]json.RawMessage{}, FetchedAt: time.Now()}
	for _, raw := range parsed.Chat {
		var e catalogEntry
		if err := json.Unmarshal(raw, &e); err != nil || e.Key == "" {
			continue
		}
		// model_config 原样保留：转发时需回传完整配置。
		cat.Configs[e.Key] = raw

		enabled := e.Enable == nil || *e.Enable
		display := e.DisplayName
		if display == "" {
			display = e.Key
		}
		ctxLen := intFromNumber(e.MaxInputTokens)
		if ctxLen <= 0 {
			ctxLen = 131072
		}
		model := Model{
			Key:             e.Key,
			Name:            display,
			ContextLength:   ctxLen,
			IsVL:            e.IsVL,
			IsReasoning:     e.IsReasoning,
			MaxOutputTokens: intFromNumber(e.MaxOutputTokens),
			Description:     e.Description,
			Hidden:          !enabled,
		}
		cat.Models = append(cat.Models, model)
	}
	if len(cat.Models) == 0 {
		return nil, fmt.Errorf("qoder model list: upstream returned no models")
	}
	sort.SliceStable(cat.Models, func(i, j int) bool { return cat.Models[i].Key < cat.Models[j].Key })
	return cat, nil
}

// Catalog 取模型目录：命中缓存直接返回；同凭据并发只打一次上游。
// force=true 跳过缓存（「拉取模型」按钮走这条）。
func (r *Runtime) Catalog(ctx context.Context, client Doer, rc Resolved, force bool) (*Catalog, error) {
	client = doerOrDefault(client)
	key := rc.cacheKey()
	now := r.now()

	if !force {
		r.mu.Lock()
		if c, ok := r.catalogs[key]; ok && c.expiresAt.After(now) {
			cat := c.catalog
			r.mu.Unlock()
			return cat, nil
		}
		if f, ok := r.flights[key]; ok {
			r.mu.Unlock()
			select {
			case <-f.done:
				return f.catalog, f.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		flight := &catalogFlight{done: make(chan struct{})}
		r.flights[key] = flight
		r.mu.Unlock()
		return r.runCatalogFlight(ctx, client, rc, key, flight)
	}

	// force：不合并、不读缓存，但仍然写回缓存（顺带刷新转发路径的 model_config）。
	flight := (*catalogFlight)(nil)
	r.mu.Lock()
	delete(r.catalogs, key)
	r.mu.Unlock()
	return r.runCatalogFlight(ctx, client, rc, key, flight)
}

// runCatalogFlight 执行一次目录拉取并写缓存。
func (r *Runtime) runCatalogFlight(ctx context.Context, client Doer, rc Resolved, key string, flight *catalogFlight) (*Catalog, error) {
	cat, err := fetchCatalog(ctx, client, rc)
	now := r.now()
	r.mu.Lock()
	if err == nil {
		r.catalogs[key] = cachedCatalog{expiresAt: now.Add(catalogTTL), catalog: cat}
	}
	if flight != nil {
		flight.catalog, flight.err = cat, err
		delete(r.flights, key)
	}
	r.mu.Unlock()
	if flight != nil {
		close(flight.done)
	}
	return cat, err
}

// InvalidateCatalog 丢弃某个凭据的目录缓存（凭据轮换 / 用户手动刷新）。
func (r *Runtime) InvalidateCatalog(rc Resolved) {
	r.mu.Lock()
	delete(r.catalogs, rc.cacheKey())
	r.mu.Unlock()
}

// ---- 工具 ----

// Doer HTTP 执行器（*http.Client 或测试替身）。
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// doerOrDefault 归一 HTTP 执行器。
// 注意 typed-nil：把一个 nil 的 *http.Client 装进接口后接口本身不是 nil，
// 直接调用会 panic —— 这里统一把 typed-nil 换成 http.DefaultClient。
func doerOrDefault(d Doer) Doer {
	if d == nil {
		return http.DefaultClient
	}
	if c, ok := d.(*http.Client); ok && c == nil {
		return http.DefaultClient
	}
	return d
}

// intFromNumber json.Number → int（非法/空得 0）。
func intFromNumber(n json.Number) int {
	if n == "" {
		return 0
	}
	v, err := n.Int64()
	if err != nil {
		f, ferr := n.Float64()
		if ferr != nil {
			return 0
		}
		return int(f)
	}
	return int(v)
}

// truncate 截断字符串（错误摘要用，绝不整段落日志）。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// UpstreamError 上游返回非 2xx 时的错误（relay 据此分类 failover）。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("qoder upstream status %d", e.Status)
	}
	return fmt.Sprintf("qoder upstream status %d: %s", e.Status, e.Body)
}
