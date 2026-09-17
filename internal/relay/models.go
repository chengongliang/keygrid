package relay

import (
	"net/http"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/server/middleware"
)

// models.go GET /v1/models —— 聚合本人渠道的模型列表（按 key 的渠道/模型绑定过滤）。

func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	k := middleware.APIKeyObj(r.Context())
	var providerIDs []int64
	if k != nil {
		providerIDs = k.ProviderIDs()
	}
	names, err := h.Op.ListUserModels(userID, providerIDs)
	if err != nil {
		gatewayError(w, protoOpenAI, http.StatusInternalServerError, "list models failed")
		return
	}
	// key 级模型白名单：只返回白名单内模型（与转发拦截保持一致）
	names = filterModelsByLimit(names, k)
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

// filterModelsByLimit 剔除不在 key 模型白名单内的模型（k 为 nil 或未设限制 = 全部保留）。
func filterModelsByLimit(names []string, k *model.ApiKey) []string {
	if k == nil || k.ModelLimit == "" {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if middleware.ModelAllowed(k.ModelLimit, n) {
			out = append(out, n)
		}
	}
	return out
}
