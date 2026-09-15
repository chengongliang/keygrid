package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/go-chi/chi/v5"
)

// AdminPricesHandler 计费：admin 全局模型价格表管理。
// 匹配规则：精确模型名 > "*" 兜底行 > 未定价（cost=0，免费放行且不计入额度累计）。
type AdminPricesHandler struct {
	Op *op.Op
	// SyncURL 价格同步上游地址（OpenRouter 公开 models API）；空 = 生产默认。
	// 测试注入 httptest URL 用；非用户输入，不走 ssrf 白名单。
	SyncURL string
	// SyncHTTPC 同步出网客户端；nil = httpx 共享 client（30s 超时，默认走平台
	// proxy_url 代理，未配置直连）。
	SyncHTTPC *http.Client
}

// GET /api/admin/prices —— 全量价格表（按模型名排序）。
func (h *AdminPricesHandler) List(w http.ResponseWriter, _ *http.Request) {
	ps, err := h.Op.ListModelPrices()
	if err != nil {
		resp.Internal(w, "list prices failed")
		return
	}
	if ps == nil {
		ps = []model.ModelPrice{}
	}
	resp.Success(w, ps)
}

// upsertPriceReq upsert 请求体：model 存在则更新，否则新增。
type upsertPriceReq struct {
	Model           string  `json:"model"`
	PromptPrice     float64 `json:"prompt_price"`     // USD / 1M tokens
	CompletionPrice float64 `json:"completion_price"` // USD / 1M tokens
	Remark          string  `json:"remark"`
}

// POST /api/admin/prices —— 按 model upsert。
func (h *AdminPricesHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	var req upsertPriceReq
	if err := resp.Decode(r, &req); err != nil {
		resp.BadRequest(w, "invalid json body")
		return
	}
	m := strings.TrimSpace(req.Model)
	if m == "" {
		resp.BadRequest(w, "model required")
		return
	}
	if req.PromptPrice < 0 || req.CompletionPrice < 0 {
		resp.BadRequest(w, "价格不能为负数")
		return
	}
	p := &model.ModelPrice{
		Model:           m,
		PromptPrice:     req.PromptPrice,
		CompletionPrice: req.CompletionPrice,
		Remark:          strings.TrimSpace(req.Remark),
	}
	if err := h.Op.UpsertModelPrice(p); err != nil {
		resp.Internal(w, "save price failed")
		return
	}
	middleware.Audit(h.Op, middleware.UserID(r.Context()), middleware.AuditEventAdminPriceUpsert,
		"model="+m+" prompt="+strconv.FormatFloat(req.PromptPrice, 'f', -1, 64)+
			" completion="+strconv.FormatFloat(req.CompletionPrice, 'f', -1, 64),
		middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, p)
}

// DELETE /api/admin/prices/{id} —— 删除定价（该模型回到"未定价 = 免费"语义）。
func (h *AdminPricesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	var target model.ModelPrice
	if err := h.Op.DB.First(&target, id).Error; err != nil {
		resp.NotFound(w, "price not found")
		return
	}
	if err := h.Op.DeleteModelPrice(id); err != nil {
		resp.Internal(w, "delete price failed")
		return
	}
	middleware.Audit(h.Op, middleware.UserID(r.Context()), middleware.AuditEventAdminPriceDelete,
		"model="+target.Model, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"deleted": true})
}

// GET /api/admin/prices/unpriced —— 渠道已用但未定价的模型（提醒列表）。
// 数据源 = 全平台 enabled 渠道 model_map keys - 价格表；无 map 的渠道枚举不出，结果非完整。
func (h *AdminPricesHandler) Unpriced(w http.ResponseWriter, _ *http.Request) {
	names, err := h.Op.ListAllUserModelNames()
	if err != nil {
		resp.Internal(w, "list model names failed")
		return
	}
	ps, err := h.Op.ListModelPrices()
	if err != nil {
		resp.Internal(w, "list prices failed")
		return
	}
	priced := map[string]bool{}
	for _, p := range ps {
		priced[p.Model] = true
	}
	var missing []string
	for _, n := range names {
		if !priced[n] {
			missing = append(missing, n)
		}
	}
	if missing == nil {
		missing = []string{}
	}
	resp.Success(w, missing)
}

// PricesHandler 用户侧只读价格表（SessionAuth）：渠道编辑页"计费名映射"下拉、
// 用量页费用说明用。价格对用户透明；写入只走 admin 端。
type PricesHandler struct {
	Op *op.Op
}

// GET /api/prices —— 只读价格表（按模型名排序）。
func (h *PricesHandler) List(w http.ResponseWriter, _ *http.Request) {
	ps, err := h.Op.ListModelPrices()
	if err != nil {
		resp.Internal(w, "list prices failed")
		return
	}
	if ps == nil {
		ps = []model.ModelPrice{}
	}
	resp.Success(w, ps)
}
