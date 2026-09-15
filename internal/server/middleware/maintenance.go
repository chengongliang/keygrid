package middleware

import (
	"encoding/json"
	"net/http"

	"github.com/chengongliang/keygrid/internal/op"
)

// MaintenanceGate 维护模式 gate —— 开启时 /v1 转发面整体 503。
// 必须挂在 ApiKeyAuth 之前：维护中即使请求带无效/缺失 key 也应得 503+公告，
// 而不是先被 401 拦截。管理端 /api/* 不挂此中间件，维护期间仍可操作。
// relay handler 内部的同类检查保留，作为纵深防御（例如 router 未挂 gate 的场景）。
func MaintenanceGate(o *op.Op) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o != nil {
				if mm, err := o.GetMaintenanceMode(); err == nil && mm {
					maintText, _ := o.GetSetting("announcement")
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]any{
							"message":     "platform under maintenance" + maintSuffix(maintText),
							"type":        "maintenance",
							"maintenance": true,
						},
					})
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// maintSuffix 维护公告拼接（空公告不加尾巴）。
func maintSuffix(text string) string {
	if text == "" {
		return ""
	}
	return ": " + text
}
