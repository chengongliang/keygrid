package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// admin_prices_sync.go 模型定价一键同步（源：OpenRouter 公开 models API，免鉴权）。
// 两步流程：preview 拉上游并对比现表（只读不改库）→ commit 批量写入勾选项。
// 单位换算：OpenRouter 价格为 USD/token（字符串），×1e6 → 本表 USD/1M tokens；
// 模型名取 id 最后一段（openai/gpt-4o → gpt-4o），与价格表「入口/标准名」口径对齐。

const (
	// openRouterModelsURL 生产同步源（固定白名单，不接受用户输入 URL）。
	openRouterModelsURL = "https://openrouter.ai/api/v1/models"
	// syncHTTPTimeout 上游拉取超时：models 列表数 MB 量级，给宽一些。
	syncHTTPTimeout = 30 * time.Second
	// syncMaxItems 单次导入上限（OpenRouter 全量几百条，给冗余）。
	syncMaxItems = 2000
	// syncRemarkDateLayout 备注里同步日期的格式。
	syncRemarkDateLayout = "2006-01-02"
)

// orModelsPayload OpenRouter models API 响应（只取需要的字段）。
type orModelsPayload struct {
	Data []orModelEntry `json:"data"`
}

type orModelEntry struct {
	ID      string         `json:"id"` // 如 "openai/gpt-4o"
	Pricing orModelPricing `json:"pricing"`
}

type orModelPricing struct {
	Prompt     string `json:"prompt"`     // USD / token，如 "0.0000025"
	Completion string `json:"completion"` // USD / token
}

// modelName id 最后一段作为价格表模型名："openai/gpt-4o" → "gpt-4o"。
func (e orModelEntry) modelName() string {
	id := strings.TrimSpace(e.ID)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return strings.TrimSpace(id[i+1:])
	}
	return id
}

// orPrice 解析后的单条上游价格（USD/1M tokens）。
type orPrice struct {
	Model           string
	PromptPrice     float64
	CompletionPrice float64
	SourceID        string // OpenRouter 原始 id，预览列表溯源展示用
}

// parseOpenRouterPrices 解析上游响应（纯函数，便于单测）：
// - 价格缺失/非法/负数的条目跳过；模型名为空跳过；同名（映射后）先到先得；
// - ×1e6 换算后 round 到 12 位小数，消除浮点乘法尾差。
func parseOpenRouterPrices(body []byte) ([]orPrice, error) {
	var payload orModelsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("OpenRouter 响应解析失败: %w", err)
	}
	seen := map[string]bool{}
	out := make([]orPrice, 0, len(payload.Data))
	for _, e := range payload.Data {
		name := e.modelName()
		if name == "" || seen[name] {
			continue
		}
		p, err := strconv.ParseFloat(strings.TrimSpace(e.Pricing.Prompt), 64)
		if err != nil || p < 0 {
			continue
		}
		c, err := strconv.ParseFloat(strings.TrimSpace(e.Pricing.Completion), 64)
		if err != nil || c < 0 {
			continue
		}
		seen[name] = true
		out = append(out, orPrice{
			Model:           name,
			PromptPrice:     round12(p * 1e6),
			CompletionPrice: round12(c * 1e6),
			SourceID:        strings.TrimSpace(e.ID),
		})
	}
	return out, nil
}

// round12 消除 USD/token ×1e6 的浮点尾差（如 2.5000000000000004 → 2.5）。
func round12(v float64) float64 { return math.Round(v*1e12) / 1e12 }

// fetchOpenRouterPrices 拉取并解析上游价格表。出网默认走平台统一出口代理
// （系统设置 proxy_url；未配置则直连），测试可注入 SyncHTTPC 覆盖。
func (h *AdminPricesHandler) fetchOpenRouterPrices() ([]orPrice, error) {
	url := h.SyncURL
	if url == "" {
		url = openRouterModelsURL
	}
	client, err := h.syncHTTPClient()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("同步源请求失败: %w", err)
	}
	defer httpResp.Body.Close()
	// 32MB 上限：正常列表数 MB，防异常响应打爆内存
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("读取同步源响应失败: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("同步源返回 HTTP %d", httpResp.StatusCode)
	}
	return parseOpenRouterPrices(body)
}

func (h *AdminPricesHandler) syncHTTPClient() (*http.Client, error) {
	if h.SyncHTTPC != nil {
		return h.SyncHTTPC, nil
	}
	// 默认走平台统一出口代理（系统设置 proxy_url，带 30s 缓存）；未配置 = 直连。
	// OpenRouter 在部分部署网络下直连不可达，代理是常规形态；与渠道代理同一套 httpx 复用池。
	proxyURL := ""
	if h.Op != nil {
		v, err := h.Op.ProxyURL()
		if err != nil {
			return nil, fmt.Errorf("read proxy setting: %w", err)
		}
		proxyURL = v
	}
	return httpx.Client(syncHTTPTimeout, proxyURL)
}

// syncPreviewItem 预览条目：同步价 vs 现价。
type syncPreviewItem struct {
	Model             string   `json:"model"`
	SourceID          string   `json:"source_id"`
	PromptPrice       float64  `json:"prompt_price"`
	CompletionPrice   float64  `json:"completion_price"`
	Status            string   `json:"status"` // new=未定价 | update=价格有变 | same=与现价一致
	CurrentPrompt     *float64 `json:"current_prompt_price,omitempty"`
	CurrentCompletion *float64 `json:"current_completion_price,omitempty"`
}

// buildSyncPreview 用现表对比同步源，产出预览（纯函数，便于单测）。
func buildSyncPreview(src []orPrice, existing []model.ModelPrice) []syncPreviewItem {
	cur := map[string]model.ModelPrice{}
	for _, p := range existing {
		cur[p.Model] = p
	}
	out := make([]syncPreviewItem, 0, len(src))
	for _, s := range src {
		it := syncPreviewItem{
			Model:           s.Model,
			SourceID:        s.SourceID,
			PromptPrice:     s.PromptPrice,
			CompletionPrice: s.CompletionPrice,
		}
		if e, ok := cur[s.Model]; ok {
			it.CurrentPrompt = &e.PromptPrice
			it.CurrentCompletion = &e.CompletionPrice
			if priceEq(e.PromptPrice, s.PromptPrice) && priceEq(e.CompletionPrice, s.CompletionPrice) {
				it.Status = "same"
			} else {
				it.Status = "update"
			}
		} else {
			it.Status = "new"
		}
		out = append(out, it)
	}
	return out
}

// priceEq 同值判断：同源同换算路径下 1e-9 以内视为一致（防浮点尾差误报「更新」）。
func priceEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// SyncPreview GET /api/admin/prices/sync/preview —— 拉取 OpenRouter 价格并与现表对比（只读不改库）。
func (h *AdminPricesHandler) SyncPreview(w http.ResponseWriter, _ *http.Request) {
	src, err := h.fetchOpenRouterPrices()
	if err != nil {
		resp.BadGateway(w, err.Error())
		return
	}
	existing, err := h.Op.ListModelPrices()
	if err != nil {
		resp.Internal(w, "list prices failed")
		return
	}
	resp.Success(w, map[string]any{"source": "openrouter", "items": buildSyncPreview(src, existing)})
}

// syncCommitReq 确认导入请求体：只写前端勾选项。
type syncCommitReq struct {
	Items []syncCommitItem `json:"items"`
}

type syncCommitItem struct {
	Model           string  `json:"model"`
	PromptPrice     float64 `json:"prompt_price"`
	CompletionPrice float64 `json:"completion_price"`
}

// SyncCommit POST /api/admin/prices/sync —— 批量 upsert 勾选项，记一条审计。
func (h *AdminPricesHandler) SyncCommit(w http.ResponseWriter, r *http.Request) {
	var req syncCommitReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	if len(req.Items) == 0 {
		resp.BadRequest(w, "items required")
		return
	}
	if len(req.Items) > syncMaxItems {
		resp.BadRequest(w, fmt.Sprintf("单次导入条数超限（≤%d）", syncMaxItems))
		return
	}
	remark := "OpenRouter 同步 " + time.Now().Format(syncRemarkDateLayout)
	ps := make([]model.ModelPrice, 0, len(req.Items))
	seen := map[string]bool{}
	for _, it := range req.Items {
		m := strings.TrimSpace(it.Model)
		if m == "" || seen[m] { // 重复条目去重，不报错
			continue
		}
		if it.PromptPrice < 0 || it.CompletionPrice < 0 {
			resp.BadRequest(w, "价格不能为负数: "+m)
			return
		}
		seen[m] = true
		ps = append(ps, model.ModelPrice{
			Model:           m,
			PromptPrice:     round12(it.PromptPrice),
			CompletionPrice: round12(it.CompletionPrice),
			Remark:          remark,
		})
	}
	if len(ps) == 0 {
		resp.BadRequest(w, "无有效条目")
		return
	}
	if err := h.Op.UpsertModelPricesBatch(ps); err != nil {
		resp.Internal(w, "save prices failed")
		return
	}
	middleware.Audit(h.Op, middleware.UserID(r.Context()), middleware.AuditEventAdminPriceSync,
		fmt.Sprintf("count=%d source=openrouter", len(ps)), middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"saved": len(ps)})
}
