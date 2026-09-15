package relay

import (
	"encoding/json"
	"net/http"
)

// respJSON OpenAI 风格 JSON 响应（models 等只读端点用）。
func respJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
