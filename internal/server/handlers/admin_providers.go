package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/go-chi/chi/v5"
)

// AdminProvidersHandler 渠道健康总览（只读，绝不返回凭据）+ 熔断手动重置。
type AdminProvidersHandler struct {
	Op      *op.Op
	Breaker *relay.Breaker // relay 进程内熔断快照（只读引用）
}

// GET /api/admin/providers
func (h *AdminProvidersHandler) List(w http.ResponseWriter, _ *http.Request) {
	rows, err := h.Op.ListProviderHealth()
	if err != nil {
		resp.Internal(w, "list provider health failed")
		return
	}
	if rows == nil {
		rows = []op.ProviderHealthRow{}
	}
	// 注入熔断快照（进程内；无 breaker 时留空）
	for i := range rows {
		if h.Breaker == nil {
			rows[i].BreakerState = ""
			continue
		}
		state := h.Breaker.SnapshotByID(rows[i].ID)
		rows[i].BreakerState = state
	}
	resp.Success(w, rows)
}

// GET /api/admin/providers/{id}/errors?limit=100
//
// 指定渠道最近的失败请求详情（与用户端同结构）：admin 排查「某个用户的渠道
// 为什么不通」时不必登入用户账号；不返回凭据，也不返回 prompt/响应正文。
func (h *AdminProvidersHandler) Errors(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		resp.BadRequest(w, "invalid provider id")
		return
	}
	rows, err := h.Op.ListRequestErrors(0, id, false, parseLimitQuery(r.URL.Query().Get("limit"), 100, 500))
	if err != nil {
		resp.Internal(w, "list request errors failed")
		return
	}
	if rows == nil {
		rows = []op.RequestErrorRow{}
	}
	resp.Success(w, rows)
}

// POST /api/admin/providers/{id}/breaker/reset
//
// 手动恢复某渠道的熔断状态。熔断是进程内状态，运维确认上游已恢复
// （换 key、充值、供应商故障恢复）后不必重启进程。
// 只影响健康判定，不触碰凭据与渠道配置；当前无在途流式请求会被打断。
func (h *AdminProvidersHandler) ResetBreaker(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		resp.BadRequest(w, "invalid provider id")
		return
	}
	if h.Breaker == nil {
		resp.Internal(w, "breaker unavailable")
		return
	}
	// 渠道名仅用于审计留痕；渠道已删除时熔断快照可能仍在，不阻断重置
	detail := fmt.Sprintf("provider_id=%d", id)
	if h.Op != nil {
		if p, perr := h.Op.GetProviderGlobal(id); perr == nil {
			detail = fmt.Sprintf("provider_id=%d name=%s", id, p.Name)
		}
	}
	h.Breaker.Reset(id)
	middleware.Audit(h.Op, middleware.UserID(r.Context()), middleware.AuditEventAdminBreakerReset,
		detail, middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"id": id, "breaker_state": h.Breaker.SnapshotByID(id)})
}
