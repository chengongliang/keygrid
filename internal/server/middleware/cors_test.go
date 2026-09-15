package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCORSDefaultDeny 空白名单 = 默认拒绝跨域：不回 CORS 头。
func TestCORSDefaultDeny(t *testing.T) {
	h := CORS(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:5173", ""} {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("origin=%q: Access-Control-Allow-Origin = %q, want empty", origin, got)
		}
	}
}

// TestCORSAllowlist 命中白名单回显 origin；不命中不回显；尾斜杠归一化。
func TestCORSAllowlist(t *testing.T) {
	h := CORS([]string{"https://gw.example.com/", "http://127.0.0.1:5173"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cases := []struct {
		origin string
		want   string
	}{
		{"https://gw.example.com", "https://gw.example.com"}, // 配置带尾斜杠仍命中
		{"http://127.0.0.1:5173", "http://127.0.0.1:5173"},
		{"https://evil.example", ""}, // 白名单外不放行
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != tc.want {
			t.Fatalf("origin=%q: got %q, want %q", tc.origin, got, tc.want)
		}
	}
}

// TestCORSPreflight OPTIONS 预检：允许时回 CORS 头并 204；未命中仍 204 但不放行。
func TestCORSPreflight(t *testing.T) {
	h := CORS([]string{"https://gw.example.com"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("OPTIONS 预检不应进入业务 handler")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://gw.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://gw.example.com" {
		t.Fatalf("preflight allowed origin: got %q", got)
	}

	req = httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("preflight denied origin: got %q, want empty", got)
	}
}
