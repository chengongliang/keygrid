package relay

import (
	"net/http"

	"github.com/chengongliang/keygrid/internal/server/middleware"
)

// models.go GET /v1/models —— 聚合本人渠道的模型列表。

func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	names, err := h.Op.ListUserModels(userID)
	if err != nil {
		gatewayError(w, protoOpenAI, http.StatusInternalServerError, "list models failed")
		return
	}
	if names == nil {
		names = []string{}
	}
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{
			"id":       n,
			"object":   "model",
			"owned_by": "keygrid",
		})
	}
	respJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}
