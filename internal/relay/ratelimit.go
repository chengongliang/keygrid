package relay

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// ratelimit.go Redis 限流（按 api_key：RPM 滑动窗口 + 并发数）。
// Redis 不可用时 fail-open（限流是保护措施，不能反过来挡死网关）。

// RateLimiter Redis 限流器。
type RateLimiter struct {
	rdb *redis.Client
	// RPM 每分钟请求数上限；0 = 不限
	RPMPerKey int
	// ConcurrencyPerKey 每 key 并发上限；0 = 不限
	ConcurrencyPerKey int
}

func NewRateLimiter(rdb *redis.Client, rpm, concurrency int) *RateLimiter {
	return &RateLimiter{rdb: rdb, RPMPerKey: rpm, ConcurrencyPerKey: concurrency}
}

// rpmKey/concKey 限流键（按 api_key）。
func rpmKey(apiKeyID int64) string {
	return fmt.Sprintf("rl:rpm:%d:%d", apiKeyID, time.Now().Unix()/60)
}
func concKey(apiKeyID int64) string { return fmt.Sprintf("rl:conc:%d", apiKeyID) }

// Check 判断请求是否放行。返回 (release func, ok)。
// release 在请求结束时调用（用于并发数扣减）；ok=false 时 release 为 nil。
func (rl *RateLimiter) Check(ctx context.Context, apiKeyID int64) (func(), bool) {
	if rl == nil || rl.rdb == nil {
		return func() {}, true
	}

	// 1. RPM 滑动窗口（按分钟桶计数，简化实现）
	if rl.RPMPerKey > 0 {
		n, err := rl.rdb.Incr(ctx, rpmKey(apiKeyID)).Result()
		if err == nil {
			if n == 1 {
				rl.rdb.Expire(ctx, rpmKey(apiKeyID), 2*time.Minute)
			}
			if int(n) > rl.RPMPerKey {
				return func() {}, false
			}
		}
		// Redis 错误 → fail-open
	}

	// 2. 并发数限制
	if rl.ConcurrencyPerKey > 0 {
		n, err := rl.rdb.Incr(ctx, concKey(apiKeyID)).Result()
		if err == nil {
			if int(n) > rl.ConcurrencyPerKey {
				rl.rdb.Decr(ctx, concKey(apiKeyID))
				return func() {}, false
			}
		}
	}

	release := func() {
		if rl.ConcurrencyPerKey > 0 && rl.rdb != nil {
			ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if n, err := rl.rdb.Decr(ctx2, concKey(apiKeyID)).Result(); err == nil && n < 0 {
				// 自愈：防止异常路径导致负数
				rl.rdb.Set(ctx2, concKey(apiKeyID), 0, time.Minute)
			}
		}
	}
	return release, true
}

// 429 响应（OpenAI 风格）。
func rateLimitResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded (rpm or concurrency)","type":"rate_limit_error"}}`))
}
