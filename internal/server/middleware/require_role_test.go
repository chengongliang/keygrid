package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// --- RequireRole ---

func TestRequireRole(t *testing.T) {
	const secret = "test-secret"
	makeHandler := func() http.Handler {
		return SessionAuth(secret)(RequireRole("admin")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})))
	}

	h := makeHandler()

	// admin token → 200
	req := httptest.NewRequest("GET", "/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+issueTestJWTRole(secret, "42", "admin", time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin token: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// user token → 403
	h = makeHandler()
	req = httptest.NewRequest("GET", "/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+issueTestJWTRole(secret, "43", "user", time.Now().Add(time.Hour)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("user token: expected 403, got %d", rec.Code)
	}

	// 无 role claim（如 relay sk- path 注入空 role）→ 403
	h = makeHandler()
	req = httptest.NewRequest("GET", "/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+issueTestJWTRole(secret, "44", "", time.Now().Add(time.Hour)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("empty role: expected 403, got %d", rec.Code)
	}

	// 无 token → 401（SessionAuth 先拦截）
	h = makeHandler()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/admin/users", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: expected 401, got %d", rec.Code)
	}

	// ctx 直读 Role roundtrip
	if Role(WithUser(context.Background(), 1, "admin")) != "admin" {
		t.Fatal("role roundtrip failed")
	}
}

// issueTestJWTRole 与 auth_test.go 的 issueTestJWT 相同，但 role 可指定。
func issueTestJWTRole(secret, sub, role string, exp time.Time) string {
	claims := jwt.MapClaims{"sub": sub, "role": role, "exp": exp.Unix(), "iat": time.Now().Unix()}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	return tok
}
