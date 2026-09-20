package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chengongliang/keygrid/internal/relay"

	"github.com/go-chi/chi/v5"
)

// reqWithProviderID 构造带 chi 路由参数的请求（单测直接调 handler，无 router）。
func reqWithProviderID(id string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/admin/providers/"+id+"/breaker/reset", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// 熔断重置：参数与依赖缺失必须有明确响应，不能假装成功。
func TestResetBreakerValidation(t *testing.T) {
	// breaker 未注入（relay 未初始化）→ 500
	h := &AdminProvidersHandler{}
	w := httptest.NewRecorder()
	h.ResetBreaker(w, reqWithProviderID("1"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("missing breaker must return 500, got %d", w.Code)
	}

	// 非法 id → 400
	h = &AdminProvidersHandler{Breaker: relay.NewBreaker()}
	w = httptest.NewRecorder()
	h.ResetBreaker(w, reqWithProviderID("abc"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id must return 400, got %d", w.Code)
	}
}

// 熔断中的渠道可以被 admin 手动恢复（不需要重启进程）。
func TestResetBreakerClosesChannel(t *testing.T) {
	b := relay.NewBreaker()
	for i := 0; i < 10; i++ { // 远超默认阈值（5）
		b.OnFailure(1)
	}
	if ok, _ := b.Allow(1); ok {
		t.Fatal("precondition: channel must be open")
	}

	h := &AdminProvidersHandler{Breaker: b}
	w := httptest.NewRecorder()
	h.ResetBreaker(w, reqWithProviderID("1"))
	if w.Code != http.StatusOK {
		t.Fatalf("reset must return 200, got %d", w.Code)
	}
	if ok, probe := b.Allow(1); !ok || probe != 0 {
		t.Fatalf("reset must close the breaker, got ok=%v probe=%d", ok, probe)
	}
}
