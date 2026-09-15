package handlers

import (
	"net/http"
	"strconv"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// audit.go GET /api/audit —— 本人审计日志（分页）。
type AuditHandler struct {
	Op *op.Op
}

// GET /api/audit?page=&size=
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	page, size := 1, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
		page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil {
		size = v
	}
	logs, total, err := h.Op.ListAuditLogs(userID, page, size)
	if err != nil {
		resp.Internal(w, "list audit failed")
		return
	}
	if logs == nil {
		logs = []model.AuditLog{}
	}
	resp.Success(w, map[string]any{"items": logs, "total": total})
}
