package handlers

import (
	"net/http"

	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// AdminProvidersHandler 渠道健康总览（只读，绝不返回凭据）。
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
