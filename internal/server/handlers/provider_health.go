package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/go-chi/chi/v5"
)

// provider_health.go 用户端「渠道可用性」：健康快照 + 失败详情 + 熔断手动重置。
//
// 与 admin 渠道健康总览共用同一套查询口径（op.ListProviderHealthForUser 内部
// 复用 admin 版 SQL），但所有访问都按当前用户的 user_id 收窄 —— 只能看到与操作
// 自己的渠道（数据隔离硬性规则 1）。

// GET /api/providers/health
//
// 返回本人全部渠道的健康行（含 24h 请求/失败/成功率/平均延迟与熔断状态），
// 供渠道页的「健康」视图展示并快速启停。
func (h *ProviderHandler) Health(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	rows, err := h.Op.ListProviderHealthForUser(userID)
	if err != nil {
		resp.Internal(w, "list provider health failed")
		return
	}
	if rows == nil {
		rows = []op.ProviderHealthRow{}
	}
	for i := range rows {
		if h.Breaker == nil {
			rows[i].BreakerState = ""
			continue
		}
		rows[i].BreakerState = h.Breaker.SnapshotByID(rows[i].ID)
	}
	resp.Success(w, rows)
}

// GET /api/providers/{id}/errors?limit=100
//
// 该渠道最近的失败请求详情（分类 / 状态码 / 上游错误摘要 / 耗时），用于排查
// 「为什么这个渠道不通」。记录会在 7 天后由后台任务清理。
func (h *ProviderHandler) Errors(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		resp.BadRequest(w, "invalid provider id")
		return
	}
	// 归属校验：不是自己的渠道一律 404（不泄露存在性）
	if _, err := h.Op.GetProvider(userID, id); err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	rows, err := h.Op.ListRequestErrors(userID, id, true, parseLimitQuery(r.URL.Query().Get("limit"), 100, 500))
	if err != nil {
		resp.Internal(w, "list request errors failed")
		return
	}
	if rows == nil {
		rows = []op.RequestErrorRow{}
	}
	resp.Success(w, rows)
}

// POST /api/providers/{id}/breaker/reset
//
// 用户手动恢复自己渠道的熔断状态（进程内状态，与 admin 版同语义）：
// 换了 key / 供应商恢复后不必等待熔断冷却或重启进程。
func (h *ProviderHandler) ResetBreaker(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		resp.BadRequest(w, "invalid provider id")
		return
	}
	p, err := h.Op.GetProvider(userID, id)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	if h.Breaker == nil {
		resp.Internal(w, "breaker unavailable")
		return
	}
	h.Breaker.Reset(id)
	middleware.Audit(h.Op, userID, middleware.AuditEventProviderBreakerReset,
		fmt.Sprintf("provider_id=%d name=%s", id, p.Name), middleware.ClientIP(r), r.UserAgent())
	resp.Success(w, map[string]any{"id": id, "breaker_state": h.Breaker.SnapshotByID(id)})
}

// parseLimitQuery 解析 limit 查询参数（缺省/非法用 def，上限 max）。
func parseLimitQuery(raw string, def, max int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
