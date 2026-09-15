package relay

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
)

// State 熔断状态机（per-channel，进程内）。
// closed → (连续 N 次失败) → open(30s) → half-open → closed/open
type State struct {
	mu sync.Mutex

	// 调参（默认 N=5, open=30s, half-open 试探 1 发）
	FailThreshold int
	OpenDuration  time.Duration

	failures   int
	state      string // closed | open | half-open
	openedAt   time.Time
	halfProbes int // half-open 下在途试探数
}

func NewState() *State {
	return &State{
		FailThreshold: defaultFailThreshold,
		OpenDuration:  defaultOpenDuration,
		state:         "closed",
	}
}

const (
	defaultFailThreshold = 5
	defaultOpenDuration  = 30 * time.Second
)

func (s *State) currentState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionLocked()
}

// transitionLocked 惰性状态转移：open 到期自动进入 half-open。
func (s *State) transitionLocked() string {
	if s.state == "open" && time.Since(s.openedAt) >= s.OpenDuration {
		s.state = "half-open"
		s.halfProbes = 0
	}
	return s.state
}

// Allow 返回是否放行该渠道。half-open 下仅放行 1 个试探请求。
func (s *State) Allow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.transitionLocked() {
	case "closed":
		return true
	case "half-open":
		if s.halfProbes > 0 {
			return false
		}
		s.halfProbes++
		return true
	default: // open
		return false
	}
}

// OnSuccess 记录成功 → closed（清零失败计数）。
func (s *State) OnSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = 0
	s.state = "closed"
	s.halfProbes = 0
}

// OnFailure 记录失败 → 达到阈值熔断 open。
func (s *State) OnFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == "half-open" {
		// 试探失败 → 重新熔断
		s.trip()
		return
	}
	if s.state != "closed" {
		return
	}
	s.failures++
	if s.failures >= s.FailThreshold {
		s.trip()
	}
}

func (s *State) trip() {
	s.state = "open"
	s.openedAt = time.Now()
	s.failures = 0
	s.halfProbes = 0
}

// Snapshot for debugging/tests.
func (s *State) Snapshot() (state string, failures int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionLocked(), s.failures
}

// SnapshotByID 读取指定渠道的熔断状态（admin 渠道健康页用）。
func (b *Breaker) SnapshotByID(providerID int64) string {
	st, _ := b.get(providerID).Snapshot()
	return st
}

func (s *State) String() string {
	st, f := s.Snapshot()
	return fmt.Sprintf("%s(failures=%d)", st, f)
}

// Breaker 渠道熔断注册表（进程内；单实例网关够用，后续可上 Redis）。
type Breaker struct {
	mu       sync.Mutex
	chans    map[int64]*State
	NewState func() *State // 可注入便于测试
}

func NewBreaker() *Breaker {
	return &Breaker{chans: map[int64]*State{}}
}

func (b *Breaker) get(providerID int64) *State {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.chans[providerID]
	if !ok {
		newFn := b.NewState
		if newFn == nil {
			newFn = NewState
		}
		st = newFn()
		b.chans[providerID] = st
	}
	return st
}

// Allow reports whether requests to the channel may proceed.
func (b *Breaker) Allow(providerID int64) bool { return b.get(providerID).Allow() }

func (b *Breaker) OnSuccess(providerID int64) { b.get(providerID).OnSuccess() }
func (b *Breaker) OnFailure(providerID int64) { b.get(providerID).OnFailure() }

// Upstream 上的熔断 + 超时控制。
func newUpstreamClient(timeout time.Duration) *http.Client {
	t := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout, // 对流式响应，timeout 覆盖整个响应周期；流式走固定 10min
		Transport: t,
	}
}

// clientFor 按渠道代理开关返回上游 client（转发主循环 + 探测共用语义）：
//   - 未勾选代理 → 默认直连 client（h.HTTPClient，保留部署级 ProxyFromEnvironment 行为）；
//   - 勾选代理 → 走平台统一代理（httpx.Client 按 (timeout, proxy) 复用连接池）；
//     平台未配置代理时返回错误，调用方 failover 该渠道 —— 不静默直连，
//     避免被墙站点直连白耗整个上游超时。
func (h *Handler) clientFor(p *model.Provider) (*http.Client, error) {
	if !p.UseProxy {
		return h.HTTPClient, nil
	}
	if h.Op == nil {
		return nil, fmt.Errorf("channel %s requires proxy, but platform proxy is not configured", p.Name)
	}
	proxyURL, err := h.Op.ProxyURL()
	if err != nil {
		return nil, fmt.Errorf("channel %s: read proxy setting: %w", p.Name, err)
	}
	if proxyURL == "" {
		return nil, fmt.Errorf("channel %s requires proxy, but platform proxy is not configured", p.Name)
	}
	return httpx.Client(upstreamTimeout, proxyURL)
}
