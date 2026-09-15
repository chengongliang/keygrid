package resp

import (
	"encoding/json"
	"io"
	"net/http"
)

// JSON writes a JSON response with the given status code.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Success 统一成功响应 {code:0, data:...}
func Success(w http.ResponseWriter, data any) {
	JSON(w, http.StatusOK, map[string]any{"code": 0, "data": data})
}

// Error 统一错误响应 {code:xxx, message:...}
func Error(w http.ResponseWriter, status, code int, message string) {
	JSON(w, status, map[string]any{"code": code, "message": message})
}

func BadRequest(w http.ResponseWriter, msg string)   { Error(w, http.StatusBadRequest, 400, msg) }
func Unauthorized(w http.ResponseWriter, msg string) { Error(w, http.StatusUnauthorized, 401, msg) }
func NotFound(w http.ResponseWriter, msg string)     { Error(w, http.StatusNotFound, 404, msg) }
func Internal(w http.ResponseWriter, msg string)     { Error(w, http.StatusInternalServerError, 500, msg) }
func BadGateway(w http.ResponseWriter, msg string)   { Error(w, http.StatusBadGateway, 502, msg) }

// Decode parses a JSON request body into dst (limited to 4 MiB).
func Decode(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	return dec.Decode(dst)
}
