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
//
// closed → (滑动窗口 FailWindow 内失败 ≥ FailThreshold) → open(OpenDuration)
//
//	→ half-open（放行 1 个带租约的探测）→ closed / open
//
// 失败计数用滑动窗口而非累计：跨小时级的零星失败不会凑成阈值，
// 只有"短时间内的连续故障"才认定为渠道不健康。
//
// 关键不变量：half-open 的探测资格最终必须归还。调用方拿到探测令牌后，
// 无论从哪条 return/continue 路径退出，都要 OnSuccess / OnFailure / ReleaseOne。
// 即使调用方漏了，探测租约（ProbeTimeout）到期会自动蒸发并重新签发，
// 渠道不会被永久卡在 half-open（历史缺陷：一条客户端断开的探测会让渠道
// 之后再无请求被放行，直到进程重启）。
type State struct {
	mu sync.Mutex

	// 调参（默认：30s 窗口内 5 次失败 → 熔断 30s；探测租约 2min）
	FailThreshold int
	FailWindow    time.Duration
	OpenDuration  time.Duration
	ProbeTimeout  time.Duration

	now func() time.Time // 可注入时钟（测试用；nil = time.Now）

	fails    []time.Time // 滑动窗口内的失败时间戳（升序）
	state    string      // closed | open | half-open
	openedAt time.Time

	probeSeq   uint64    // 已签发的探测令牌序号
	probeToken uint64    // 在途探测令牌（0 = 无在途探测）
	probeAt    time.Time // 在途探测的签发时间（租约起点）
}

func NewState() *State {
	return &State{
		FailThreshold: defaultFailThreshold,
		FailWindow:    defaultFailWindow,
		OpenDuration:  defaultOpenDuration,
		ProbeTimeout:  defaultProbeTimeout,
		state:         "closed",
		now:           time.Now,
	}
}

const (
	defaultFailThreshold = 5
	defaultFailWindow    = 30 * time.Second
	defaultOpenDuration  = 30 * time.Second
	defaultProbeTimeout  = 2 * time.Minute
)

func (s *State) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// failWindow/openDuration/probeTimeout 带默认值兜底：允许测试直接构造 &State{...}。
func (s *State) failWindow() time.Duration {
	if s.FailWindow <= 0 {
		return defaultFailWindow
	}
	return s.FailWindow
}

func (s *State) openDuration() time.Duration {
	if s.OpenDuration <= 0 {
		return defaultOpenDuration
	}
	return s.OpenDuration
}

func (s *State) probeTimeout() time.Duration {
	if s.ProbeTimeout <= 0 {
		return defaultProbeTimeout
	}
	return s.ProbeTimeout
}

// currentState 读取（并惰性推进）当前状态。
func (s *State) currentState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionLocked()
}

// transitionLocked 惰性状态转移：open 到期自动进入 half-open（清空上轮探测）。
func (s *State) transitionLocked() string {
	if s.state == "open" && s.clock().Sub(s.openedAt) >= s.openDuration() {
		s.state = "half-open"
		s.clearProbeLocked()
	}
	return s.state
}

// Allow 返回是否放行该渠道，以及 half-open 下的探测令牌。
//
// 令牌非零 = 本次请求持有 half-open 探测资格，调用方必须在本次渠道尝试结束时
// 调用一次 OnSuccess / OnFailure / ReleaseOne(令牌) 归还它。closed 状态放行时
// 令牌为 0（无需归还）；open 状态返回 (false, 0)。
//
// half-open 下有在途探测时拒绝其它请求；若在途探测超过租约（ProbeTimeout）
// 仍未归还（调用方漏调或请求挂死），视为失效并签发新的探测。
func (s *State) Allow() (bool, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.transitionLocked() {
	case "closed":
		return true, 0
	case "half-open":
		if s.probeToken != 0 && s.clock().Sub(s.probeAt) < s.probeTimeout() {
			return false, 0
		}
		s.probeSeq++
		s.probeToken = s.probeSeq
		s.probeAt = s.clock()
		return true, s.probeToken
	default: // open
		return false, 0
	}
}

// ReleaseOne 归还探测资格（幂等）。仅当令牌仍是当前在途探测时生效：
// 探测已由 OnSuccess/OnFailure 收敛时不产生任何影响，避免误伤后续探测。
func (s *State) ReleaseOne(token uint64) {
	if token == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probeToken == token {
		s.clearProbeLocked()
	}
}

// OnSuccess 记录成功 → closed（清空失败窗口 + 探测资格）。
func (s *State) OnSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = nil
	s.state = "closed"
	s.clearProbeLocked()
}

// OnFailure 记录失败 → 滑动窗口内达到阈值则熔断 open。
func (s *State) OnFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if s.state == "half-open" {
		// 探测失败 → 立即重新熔断（半开失败的信号足够强，不再走窗口计数）
		s.tripLocked(now)
		return
	}
	if s.state != "closed" {
		return
	}
	s.fails = append(s.fails, now)
	s.trimLocked(now)
	if len(s.fails) >= s.FailThreshold {
		s.tripLocked(now)
	}
}

// Reset 强制恢复 closed（admin 手动重置熔断用）。
func (s *State) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = nil
	s.state = "closed"
	s.clearProbeLocked()
}

// trimLocked 丢弃滑动窗口外的失败时间戳（fails 按时间升序）。
func (s *State) trimLocked(now time.Time) {
	cut := now.Add(-s.failWindow())
	i := 0
	for i < len(s.fails) && s.fails[i].Before(cut) {
		i++
	}
	if i > 0 {
		s.fails = append(s.fails[:0], s.fails[i:]...)
	}
}

func (s *State) tripLocked(now time.Time) {
	s.state = "open"
	s.openedAt = now
	s.fails = nil
	s.clearProbeLocked()
}

func (s *State) clearProbeLocked() {
	s.probeToken = 0
	s.probeAt = time.Time{}
}

// Snapshot for debugging/tests.
func (s *State) Snapshot() (state string, failures int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.transitionLocked()
	return st, len(s.fails)
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

// Allow reports whether requests to the channel may proceed, plus the half-open
// probe token（非零时调用方必须 ReleaseOne 或 OnSuccess/OnFailure）。
func (b *Breaker) Allow(providerID int64) (bool, uint64) { return b.get(providerID).Allow() }

// ReleaseOne 归还 half-open 探测资格（未决探测的兜底释放；幂等）。
func (b *Breaker) ReleaseOne(providerID int64, token uint64) {
	b.get(providerID).ReleaseOne(token)
}

func (b *Breaker) OnSuccess(providerID int64) { b.get(providerID).OnSuccess() }
func (b *Breaker) OnFailure(providerID int64) { b.get(providerID).OnFailure() }

// Reset 手动恢复某渠道（admin 重置熔断）。
func (b *Breaker) Reset(providerID int64) { b.get(providerID).Reset() }

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
