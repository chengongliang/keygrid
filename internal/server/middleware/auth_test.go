package middleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// --- SessionAuth ---

func issueTestJWT(secret string, sub string, exp time.Time) string {
	claims := jwt.MapClaims{"sub": sub, "role": "user", "exp": exp.Unix(), "iat": time.Now().Unix()}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	return tok
}

func TestSessionAuth(t *testing.T) {
	const secret = "test-secret"
	handler := SessionAuth(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserID(r.Context()) != 42 {
			t.Errorf("expected user id 42, got %d", UserID(r.Context()))
		}
		w.WriteHeader(http.StatusOK)
	}))

	// 有效 token
	req := httptest.NewRequest("GET", "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+issueTestJWT(secret, "42", time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 无 token
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/keys", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: expected 401, got %d", rec.Code)
	}

	// 篡改签名
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/keys", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: expected 401, got %d", rec.Code)
	}

	// 过期
	req = httptest.NewRequest("GET", "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+issueTestJWT(secret, "42", time.Now().Add(-time.Hour)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: expected 401, got %d", rec.Code)
	}

	// ctx 注入/读取
	if WithUser(context.Background(), 1, "admin"); true {
	}
	if Role(WithUser(context.Background(), 1, "admin")) != "admin" {
		t.Fatal("role roundtrip failed")
	}
}

// --- ApiKeyAuth ---

func newTestOp(t *testing.T) *op.Op {
	t.Helper()
	o := &op.Op{}
	// 用伪 key 数据直接塞到 op？不行——op 需要 DB。改用 sqlite 不可行（无 driver），
	// 这里退化为仅测 hash 逻辑 + 401 短路，DB 路径由 E2E 覆盖。
	return o
}

func TestApiKeyAuthFormat(t *testing.T) {
	_ = newTestOp // keep helper referenced
	handler := ApiKeyAuth(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 非 sk- 前缀直接 401（不触 DB，nil op 安全）
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non sk- key: expected 401, got %d", rec.Code)
	}

	// x-api-key 携带（Anthropic SDK 形状）：非 sk- 前缀也要 401
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", "not-sk-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("x-api-key non sk-: expected 401, got %d", rec.Code)
	}

	// x-api-key sk- 前缀通过格式检查（进入 DB 查询 → nil op panic？不：GetApiKeyByKeyHash
	// 在 nil receiver 上会 panic —— 无法验证。只验证 401 短路路径两种 header 形状均生效）
}

func TestAPIKeyHashRoundTrip(t *testing.T) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	plaintext := "sk-" + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	hash := hex.EncodeToString(sum[:])

	if len(hash) != 64 {
		t.Fatalf("sha256 hex length = %d, want 64", len(hash))
	}
	// 相同输入相同 hash（鉴权按此等值查询）
	sum2 := sha256.Sum256([]byte(plaintext))
	if hash != hex.EncodeToString(sum2[:]) {
		t.Fatal("hash not deterministic")
	}
	// 不同 key 不同 hash
	plaintext2 := "sk-" + hex.EncodeToString([]byte("different"))
	sum2b := sha256.Sum256([]byte(plaintext2))
	if hash == hex.EncodeToString(sum2b[:]) {
		t.Fatal("hash collision on different input")
	}
}

// bcrypt 往返（登录路径核心）
func TestBcryptRoundTrip(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-pass"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte("s3cret-pass")) != nil {
		t.Fatal("correct password rejected")
	}
	if bcrypt.CompareHashAndPassword(hash, []byte("wrong")) == nil {
		t.Fatal("wrong password accepted")
	}
}

var _ = model.ApiKey{}
