package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/golang-jwt/jwt/v5"
)

type ctxKey int

const (
	ctxUserID ctxKey = iota + 1
	ctxRole
	ctxAPIKeyID
	ctxAPIKeyObj
)

// WithUser injects user identity into ctx.
func WithUser(ctx context.Context, userID int64, role string) context.Context {
	ctx = context.WithValue(ctx, ctxUserID, userID)
	if role != "" {
		ctx = context.WithValue(ctx, ctxRole, role)
	}
	return ctx
}

// WithAPIKey injects the relay api key id into ctx.
func WithAPIKey(ctx context.Context, apiKeyID int64) context.Context {
	return context.WithValue(ctx, ctxAPIKeyID, apiKeyID)
}

// WithAPIKeyObj injects the relay api key object into ctx（relay 侧模型限制等校验用）。
func WithAPIKeyObj(ctx context.Context, k *model.ApiKey) context.Context {
	return context.WithValue(ctx, ctxAPIKeyObj, k)
}

// APIKeyObj returns the sk- key object used on the relay path (nil if absent).
func APIKeyObj(ctx context.Context) *model.ApiKey {
	k, _ := ctx.Value(ctxAPIKeyObj).(*model.ApiKey)
	return k
}

// UserID returns the authenticated user id (0 if absent).
func UserID(ctx context.Context) int64 {
	id, _ := ctx.Value(ctxUserID).(int64)
	return id
}

// Role returns the authenticated user's role ("" if absent).
func Role(ctx context.Context) string {
	role, _ := ctx.Value(ctxRole).(string)
	return role
}

// APIKeyID returns the id of the sk- key used on the relay path (0 if absent).
func APIKeyID(ctx context.Context) int64 {
	id, _ := ctx.Value(ctxAPIKeyID).(int64)
	return id
}

// SessionAuth JWT Bearer 会话鉴权（Web/API 管理面用）。
func SessionAuth(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				resp.Unauthorized(w, "missing bearer token")
				return
			}
			tokenStr := strings.TrimPrefix(h, "Bearer ")

			claims := jwt.MapClaims{}
			token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, errors.New("unexpected signing method")
				}
				return []byte(secret), nil
			})
			if err != nil || !token.Valid {
				resp.Unauthorized(w, "invalid token")
				return
			}
			uid, ok := extractUserID(claims)
			if !ok {
				resp.Unauthorized(w, "invalid token claims")
				return
			}
			role, _ := claims["role"].(string)
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), uid, role)))
		})
	}
}

func extractUserID(claims jwt.MapClaims) (int64, bool) {
	switch v := claims["sub"].(type) {
	case string:
		var id int64
		_, err := fmt.Sscan(v, &id)
		return id, err == nil
	case float64:
		return int64(v), true
	}
	return 0, false
}

// RequireRole 管理面角色门槛：必须先过 SessionAuth（ctx 里有 role）。
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if Role(r.Context()) != role {
				resp.Error(w, http.StatusForbidden, 403, "admin role required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ApiKeyAuth 平台 sk- key 鉴权（relay 转发面用）。校验 enabled/过期/IP 白名单，
// 并节流更新 last_used_at（60s 内不重复写库）。
// 兼容两种携带方式：Authorization: Bearer sk-...（OpenAI 客户端）与
// x-api-key: sk-...（Anthropic SDK 客户端，/v1/messages 用）。
func ApiKeyAuth(o *op.Op) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var raw string
			if h := r.Header.Get("x-api-key"); strings.HasPrefix(h, "sk-") {
				raw = h
			} else {
				h := r.Header.Get("Authorization")
				if !strings.HasPrefix(h, "Bearer sk-") {
					resp.Unauthorized(w, "invalid api key format")
					return
				}
				raw = strings.TrimPrefix(h, "Bearer ")
			}

			sum := sha256.Sum256([]byte(raw))
			hash := hex.EncodeToString(sum[:])

			key, err := o.GetApiKeyByKeyHash(hash)
			if err != nil {
				resp.Unauthorized(w, "invalid api key")
				return
			}
			if !key.Enabled {
				resp.Unauthorized(w, "api key disabled")
				return
			}
			if key.ExpiresAt != nil && key.ExpiresAt.Before(time.Now()) {
				resp.Unauthorized(w, "api key expired")
				return
			}
			if !IPAllowed(key.IPWhitelist, ClientIP(r)) {
				resp.Error(w, http.StatusForbidden, 403, "api key ip not allowed")
				return
			}
			// last_used_at 节流：距上次 >60s 才写（避免每请求一次 UPDATE）
			if key.LastUsedAt == nil || time.Since(*key.LastUsedAt) > time.Minute {
				_ = o.TouchApiKeyLastUsed(key.ID, time.Now())
			}
			ctx := WithUser(r.Context(), key.UserID, "")
			ctx = WithAPIKey(ctx, key.ID)
			ctx = WithAPIKeyObj(ctx, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
