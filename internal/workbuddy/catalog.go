package workbuddy

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// catalog.go 上游模型目录抓取（四段回落 + 内嵌快照兜底）。
//
// 对齐 wb2a：wb_proxy.py:1803-1835（窄端点）/ 1979-2000（/v3/config），
// 设计文档 docs/workbuddy-integration-design.md §3.5。
//
// 上游事实（端点）来自 realm.go 的常量，全部是编译期固定域名，不存在
// 用户输入 URL，因此无需再过 ssrf 校验。base_url 若被用户覆盖，其 SSRF
// 校验在调用方（handler/relay）完成。

//go:embed snapshot/models_intl.json snapshot/models_cn.json
var snapshotFS embed.FS

const (
	// catalogTimeout 单次目录请求的阶段超时（在调用方 ctx 之上再收口）。
	catalogTimeout = 30 * time.Second
	// catalogMaxBytes 响应体读取上限（目录 JSON 通常远小于此）。
	catalogMaxBytes = 4 << 20
	// catalogSnapshotDir 内嵌快照目录。
	catalogSnapshotDir = "snapshot"
)

// snapshotJSON 内嵌快照文件结构。
type snapshotJSON struct {
	Realm      string   `json:"realm"`
	CapturedAt string   `json:"capturedAt"`
	Source     string   `json:"source"`
	Models     []string `json:"models"`
}

// catalogTierAliases 档位别名（并非真实模型，一律剔除）。
var catalogTierAliases = map[string]struct{}{
	"default-model":  {},
	"fast-model":     {},
	"balanced-model": {},
	"primary-model":  {},
	"deep-model":     {},
	"auto":           {},
}

// SnapshotModels 返回某区域内嵌模型快照（已应用全部过滤规则）。
// 未知区域或快照解析失败返回 nil。
func SnapshotModels(realm Realm) []string {
	name := snapshotFile(realm)
	if name == "" {
		return nil
	}
	data, err := snapshotFS.ReadFile(catalogSnapshotDir + "/" + name)
	if err != nil {
		return nil
	}
	var snap snapshotJSON
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil
	}
	return filterModelIDs(snap.Models)
}

// snapshotFile 区域 → 内嵌快照文件名。
func snapshotFile(realm Realm) string {
	switch realm {
	case RealmIntl:
		return "models_intl.json"
	case RealmCN:
		return "models_cn.json"
	default:
		return ""
	}
}

// FetchModels 拉取该渠道可用的上游模型 ID 列表。
//
// 四段回落（对齐 wb2a wb_proxy.py:1803-1835 / 1979-2000）：
//  1. GET {base}/v3/config 的 agents[cli].models —— 上游要求桌面 UA，否则 400/12403；
//  2. GET {base}/v2/enterprises/personal/models（窄端点）；
//  3. 内嵌快照 snapshot/models_{realm}.json；
//  4. 都失败时返回 error，调用方回落到预设模型列表。
func FetchModels(ctx context.Context, client *http.Client, p *model.Provider, tok *oauth.TokenSet) ([]string, error) {
	realm, ok := RealmOf(p)
	if !ok {
		return nil, ErrNotWorkBuddy
	}
	if client == nil {
		client = http.DefaultClient
	}
	if tok == nil {
		tok = &oauth.TokenSet{}
	}

	var errs []string

	// 1) /v3/config（需桌面 UA）。
	if target, err := RemoteConfigEndpoint(p); err == nil {
		if models, ferr := fetchConfigModels(ctx, client, target, p, tok); ferr != nil {
			errs = append(errs, ferr.Error())
		} else if len(models) > 0 {
			return models, nil
		}
	} else {
		errs = append(errs, err.Error())
	}

	// 2) 窄端点。
	if target, err := ModelsEndpoint(p); err == nil {
		if models, ferr := fetchNarrowModels(ctx, client, target, p, tok); ferr != nil {
			errs = append(errs, ferr.Error())
		} else if len(models) > 0 {
			return models, nil
		}
	} else {
		errs = append(errs, err.Error())
	}

	// 3) 内嵌快照。
	if snap := SnapshotModels(realm); len(snap) > 0 {
		return snap, nil
	}

	// 4) 全部失败。错误里只有状态码与结构化错误字段，绝不含 token / body 原文。
	return nil, fmt.Errorf("workbuddy model catalog unavailable: %s", strings.Join(errs, "; "))
}

// fetchConfigModels 请求 /v3/config 并解析模型 ID。
func fetchConfigModels(ctx context.Context, client *http.Client, target string, p *model.Provider, tok *oauth.TokenSet) ([]string, error) {
	body, err := catalogGET(ctx, client, target, p, tok)
	if err != nil {
		return nil, err
	}
	return parseConfigModels(body), nil
}

// fetchNarrowModels 请求窄端点并解析模型 ID。
func fetchNarrowModels(ctx context.Context, client *http.Client, target string, p *model.Provider, tok *oauth.TokenSet) ([]string, error) {
	body, err := catalogGET(ctx, client, target, p, tok)
	if err != nil {
		return nil, err
	}
	return parseNarrowModels(body), nil
}

// catalogGET 发一次带身分头的 GET（Authorization 由本函数补，Headers 不设）。
//
// 用 Headers 拿到桌面 UA + IDE 身分头（/v3/config 没有这些头会 400/12403）；
// 每个请求在调用方 ctx 之上再套 30s 超时。非 2xx 只回结构化错误字段。
func catalogGET(ctx context.Context, client *http.Client, target string, p *model.Provider, tok *oauth.TokenSet) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build catalog request: %w", err)
	}
	h, err := Headers(p, tok, HeaderOpt{Product: DefaultProduct(), Fingerprint: Fingerprint()})
	if err != nil {
		return nil, err
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read catalog response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, catalogErrSummary(body))
	}
	return body, nil
}

// parseConfigModels 防御式解析 /v3/config：形状不保证，按优先级取
//  1. data.agents[].models[]（优先 agentType/type/name == cli 的 agent）；
//  2. data.models[]；
//  3. 顶层 models[]。
//
// 返回第一个过滤后非空的来源。
func parseConfigModels(body []byte) []string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	data, _ := root["data"].(map[string]any)

	if data != nil {
		if agents, ok := data["agents"].([]any); ok {
			if ids := modelsFromAgents(agents); len(ids) > 0 {
				return ids
			}
		}
		if ids := filterModelIDs(collectModelIDs(data["models"])); len(ids) > 0 {
			return ids
		}
	}
	return filterModelIDs(collectModelIDs(root["models"]))
}

// modelsFromAgents 从 agents 数组取模型：优先第一个有模型的 cli agent，
// 否则回落到第一个有模型的 agent。
func modelsFromAgents(agents []any) []string {
	var fallback []string
	for _, a := range agents {
		m, _ := a.(map[string]any)
		if m == nil {
			continue
		}
		ids := filterModelIDs(collectModelIDs(m["models"]))
		if len(ids) == 0 {
			continue
		}
		if isCLIAgent(m) {
			return ids
		}
		if fallback == nil {
			fallback = ids
		}
	}
	return fallback
}

// isCLIAgent agentType/type/name 是否为 cli。
func isCLIAgent(m map[string]any) bool {
	for _, k := range []string{"agentType", "type", "name"} {
		if s, ok := m[k].(string); ok && strings.EqualFold(strings.TrimSpace(s), "cli") {
			return true
		}
	}
	return false
}

// parseNarrowModels 防御式解析窄端点：接受 data / models 两种容器，
// 条目可以是字符串或对象（id/modelId/name）。
func parseNarrowModels(body []byte) []string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	if ids := filterModelIDs(collectModelIDs(root["data"])); len(ids) > 0 {
		return ids
	}
	return filterModelIDs(collectModelIDs(root["models"]))
}

// collectModelIDs 从任意 JSON 值里尽力收集模型 ID：
//   - 字符串 → 作为 ID；
//   - 数组 → 逐项递归；
//   - 对象 → 取 id/modelId/name；都没有但有 models 字段则递归 models。
func collectModelIDs(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		out = append(out, t)
	case []any:
		for _, item := range t {
			out = append(out, collectModelIDs(item)...)
		}
	case map[string]any:
		if s := modelIDFromObject(t); s != "" {
			out = append(out, s)
			return out
		}
		if inner, ok := t["models"]; ok {
			out = append(out, collectModelIDs(inner)...)
		}
	}
	return out
}

// modelIDFromObject 从对象条目取模型 ID（id → modelId → name）。
func modelIDFromObject(m map[string]any) string {
	for _, k := range []string{"id", "modelId", "name"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// filterModelIDs 过滤 + 去重，保留首次出现顺序：
//   - 去空串；
//   - 去档位别名（含 default-<版本> 内部默认档）与 auto；
//   - 去 `-sg` / `-x` 区域/变体后缀；
//   - 同名 `-f`（free/0 倍率档）与 base 并存时，只留 `-f`。
func filterModelIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if isTierAlias(id) || hasVariantSuffix(id) {
			continue
		}
		out = append(out, id)
	}

	// -f 规则：先标记所有 -f 的 base，再去掉这些 base（保留免费档）。
	freeBase := make(map[string]struct{})
	for _, id := range out {
		if base, ok := strings.CutSuffix(id, "-f"); ok {
			freeBase[base] = struct{}{}
		}
	}
	if len(freeBase) == 0 {
		return out
	}
	filtered := out[:0]
	for _, id := range out {
		if _, replaced := freeBase[id]; replaced {
			continue
		}
		filtered = append(filtered, id)
	}
	return filtered
}

// isTierAlias 档位别名判定。除显式别名外，default-<版本>（default-1.1 /
// default-1.2）这类内部默认档同样不是真实模型，一并剔除。
func isTierAlias(id string) bool {
	if _, ok := catalogTierAliases[id]; ok {
		return true
	}
	return strings.HasPrefix(id, "default-")
}

// hasVariantSuffix 是否带 `-sg`（新加坡区域）/ `-x`（变体）后缀。
func hasVariantSuffix(id string) bool {
	return strings.HasSuffix(id, "-sg") || strings.HasSuffix(id, "-x")
}

// catalogErrSummary 只提取结构化错误字段（error.message / error.code / code / msg），
// 截断 200 字符；绝不回显 body 原文（上游错误体可能回显 prompt 片段）。
func catalogErrSummary(body []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "non-json error body"
	}
	var parts []string
	if e, ok := parsed["error"].(map[string]any); ok {
		if s := scalarString(e["message"]); s != "" {
			parts = append(parts, "message="+s)
		}
		if s := scalarString(e["code"]); s != "" {
			parts = append(parts, "code="+s)
		}
	}
	if s := scalarString(parsed["code"]); s != "" {
		parts = append(parts, "code="+s)
	}
	if s := scalarString(parsed["msg"]); s != "" {
		parts = append(parts, "msg="+s)
	}
	if len(parts) == 0 {
		return "unrecognized error shape"
	}
	return truncateCatalog(strings.Join(parts, " "), 200)
}

// scalarString 把 JSON 标量转成字符串（字符串原样；数字/布尔取字面值）。
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return ""
	}
}

// truncateCatalog 按字符截断，避免把大段上游文本塞进错误信息。
func truncateCatalog(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
