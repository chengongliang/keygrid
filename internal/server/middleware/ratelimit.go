package middleware

import (
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/server/resp"
)

// ratelimit.go 登录/注册限速：进程内滑动窗口，防暴力破解。
// 单实例足够；多实例部署时可通过 Redis 升级（compose 默认单实例）。

type loginAttempt struct {
	count int
	reset time.Time
}

// LoginRateLimiter 每 IP+email 组合的滑动窗口限速器。
type LoginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
	max      int           // 窗口内最大次数
	window   time.Duration // 窗口长度
}

// NewLoginRateLimiter 默认：5 次 / 分钟。
func NewLoginRateLimiter() *LoginRateLimiter {
	l := &LoginRateLimiter{
		attempts: map[string]*loginAttempt{},
		max:      5,
		window:   time.Minute,
	}
	go l.gcLoop()
	return l
}

// Allow 记录一次尝试；超限返回 false（不记入计数，防误伤）。
func (l *LoginRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a, ok := l.attempts[key]
	if !ok || now.After(a.reset) {
		l.attempts[key] = &loginAttempt{count: 1, reset: now.Add(l.window)}
		return true
	}
	if a.count >= l.max {
		return false
	}
	a.count++
	return true
}

// gcLoop 周期清理过期条目防泄漏。
func (l *LoginRateLimiter) gcLoop() {
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		now := time.Now()
		l.mu.Lock()
		for k, a := range l.attempts {
			if now.After(a.reset) {
				delete(l.attempts, k)
			}
		}
		l.mu.Unlock()
	}
}

// ClientIP 提取客户端 IP（RealIP 中间件已在前面处理 X-Forwarded-For）。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimitByIP 按客户端 IP 限速的中间件包装（登录/注册/改密等凭据相关操作）。
// E2E_MODE=1 时跳过，避免 E2E 反复注册/登录触发 429。
// 注意：勿复用 SSRF_ALLOW_INTERNAL —— 那是自托管可选的 SSRF 放行开关，
// 与登录限速无关，开着它不能顺带关掉限速。
func RateLimitByIP(l *LoginRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if os.Getenv("E2E_MODE") == "1" {
				next.ServeHTTP(w, r)
				return
			}
			ip := ClientIP(r)
			if !l.Allow("ip:" + ip) {
				Audit(nil, 0, AuditEventLoginRateHit, "too many attempts", ip, r.UserAgent())
				resp.Error(w, http.StatusTooManyRequests, 429, "too many attempts, try later")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
