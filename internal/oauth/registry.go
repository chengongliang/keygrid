package oauth

import (
	"context"
	"sync"
	"time"
)

// registry.go provider 注册表（对应 9router PROVIDER_OAUTH + flowType）。
// 每个 provider 一个 Adapter，按 key 注册；lookup 线程安全。

var (
	regMu    sync.RWMutex
	registry = map[string]Adapter{}
)

// Register 注册 adapter（key 冲突时 panic —— 启动期错误）。
func Register(a Adapter) {
	regMu.Lock()
	defer regMu.Unlock()
	k := a.Key()
	if _, dup := registry[k]; dup {
		panic("oauth: adapter registered twice: " + k)
	}
	registry[k] = a
}

// Lookup 按 key 查 adapter。
func Lookup(key string) (Adapter, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	a, ok := registry[key]
	return a, ok
}

// IsRegistered key 是否已注册（handler 校验用）。
func IsRegistered(key string) bool {
	_, ok := Lookup(key)
	return ok
}

// Keys 已注册的 provider key 列表（不稳定顺序）。
func Keys() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}

// Resolve 轮询/换 token 的宿主入口：由 server handler 调用。
// state.user_id 归属校验由调用方完成。
type ResolveInput = map[string]string

// ---- 刷新错误分类（9router classifyOAuthRefreshError 1:1）----

// permanent 标记不可恢复错误：refresh_token 失效/被重用 → 凭据必须 revoked 重授权。
var permanentMarkers = []string{
	"refresh_token_expired",
	"refresh_token_reused",
	"refresh_token_invalidated",
	"invalid_grant",
	// 9router isUnrecoverableRefreshError 另含 invalid_request
	"invalid_request",
}

// ClassifyRefreshError 归类刷新错误：permanent=true → 凭据 revoked；否则择机重试。
func ClassifyRefreshError(err error) (permanent bool, code string) {
	if err == nil {
		return false, ""
	}
	switch e := err.(type) {
	case *RefreshError:
		combined := e.OAuthError + " " + e.Body
		for _, m := range permanentMarkers {
			if containsFold(combined, m) {
				return true, m
			}
		}
		return false, e.OAuthError
	default:
		// 网络错误/超时 → 择机重试
		return false, ""
	}
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && indexOfFold(s, sub) >= 0
}

func indexOfFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// Scheduler 刷新调度器宿主接口（internal/task 后台扫描 + relay 兜底共用）。
// 实现在 refresh.go（需要 op + redis）。
type SchedulerDeps struct {
	Now func() time.Time
}

// context 别名避免各文件重复 import
var _ = context.Background
