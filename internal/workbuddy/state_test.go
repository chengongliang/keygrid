package workbuddy

// state_test.go 运行态判定器的可测性设计：
//
//   - 纯判定逻辑走 admitDecision（不碰 DB/Redis）表驱动覆盖；
//   - DB 能力通过未导出的 stateStore 接口注入内存假实现（仓库不做 DB-backed 测试）；
//   - Redis 能力通过未导出的 counterStore 接口注入内存假实现，覆盖 Redis 分支；
//   - ProbeCredits 的积分端点通过未导出的 creditsURL 接缝指向 httptest。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
)

// ---- 测试替身 ----

// fakeDB stateStore 的内存实现。
type fakeDB struct {
	settings *model.WorkbuddySetting
	state    *model.WorkbuddyChannelState

	settingErr error
	stateErr   error
	sum        int64
	sumErr     error
	creditErr  error

	settingCalls int
	stateCalls   int
	upserts      []*model.WorkbuddyChannelState
	daily        []fakeDaily
	credits      []fakeCredit
}

type fakeDaily struct {
	date  string
	delta int64
}

type fakeCredit struct {
	credits float64
	at      time.Time
}

func (f *fakeDB) GetWorkbuddySetting(userID int64) (*model.WorkbuddySetting, error) {
	f.settingCalls++
	if f.settingErr != nil {
		return nil, f.settingErr
	}
	return f.settings, nil
}

func (f *fakeDB) GetWorkbuddyChannelState(userID, providerID int64) (*model.WorkbuddyChannelState, error) {
	f.stateCalls++
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	return f.state, nil
}

func (f *fakeDB) UpsertWorkbuddyChannelState(s *model.WorkbuddyChannelState) error {
	f.upserts = append(f.upserts, s)
	f.state = s
	return nil
}

func (f *fakeDB) AddWorkbuddyDailyTokens(userID, providerID int64, date string, delta int64, now time.Time) error {
	f.daily = append(f.daily, fakeDaily{date: date, delta: delta})
	return nil
}

func (f *fakeDB) SumProviderTokens(userID, providerID int64, since, until time.Time) (int64, error) {
	return f.sum, f.sumErr
}

func (f *fakeDB) SetWorkbuddyCredits(userID, providerID int64, credits float64, at time.Time) error {
	if f.creditErr != nil {
		return f.creditErr
	}
	f.credits = append(f.credits, fakeCredit{credits: credits, at: at})
	return nil
}

// fakeCounter counterStore 的内存实现（带 TTL，能断言冷却有效期）。
type fakeCounter struct {
	mu   sync.Mutex
	now  func() time.Time
	vals map[string]fakeEntry
}

type fakeEntry struct {
	val     string
	expires time.Time // 零值 = 永不过期
}

func newFakeCounter(now func() time.Time) *fakeCounter {
	if now == nil {
		now = time.Now
	}
	return &fakeCounter{now: now, vals: map[string]fakeEntry{}}
}

func (f *fakeCounter) Get(_ context.Context, key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.vals[key]
	if !ok {
		return "", false
	}
	if !e.expires.IsZero() && !e.expires.After(f.now()) {
		delete(f.vals, key)
		return "", false
	}
	return e.val, true
}

func (f *fakeCounter) Set(_ context.Context, key, value string, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := fakeEntry{val: value}
	if ttl > 0 {
		e.expires = f.now().Add(ttl)
	}
	f.vals[key] = e
}

func (f *fakeCounter) Delete(_ context.Context, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vals, key)
}

// ttl 剩余有效期（测试断言冷却 TTL 用）。
func (f *fakeCounter) ttl(key string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.vals[key]
	if !ok || e.expires.IsZero() {
		return 0
	}
	return e.expires.Sub(f.now())
}

// ---- 公共夹具 ----

var stateTestNow = time.Date(2025, 3, 14, 10, 0, 0, 0, time.UTC)

func stateTestProvider() *model.Provider {
	return &model.Provider{ID: 7, UserID: 3, OAuthProvider: ProviderIntl}
}

// newTestRuntime 组装一个「已接线」的 Runtime（Op 非 nil 以满足公开契约，
// 实际读写走注入的假 DB / 假计数器）。
func newTestRuntime(db stateStore, cs counterStore, now time.Time) *Runtime {
	return &Runtime{
		Op:  &op.Op{},
		dbs: db,
		cs:  cs,
		Now: func() time.Time { return now },
	}
}

func parkedOf(t *testing.T, err error) *ParkedError {
	t.Helper()
	if err == nil {
		return nil
	}
	var pe *ParkedError
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *ParkedError (%v)", err, err)
	}
	return pe
}

// ---- ParkedError ----

func TestParkedErrorMessageStable(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cooldownUntil := time.Date(2025, 3, 14, 10, 30, 0, 0, cst) // 等价 UTC 02:30

	cases := []struct {
		name string
		err  *ParkedError
		want string
	}{
		{"disabled 无 Until", &ParkedError{Reason: reasonDisabled}, "workbuddy channel parked: disabled"},
		{"reserve_credits 无 Until", &ParkedError{Reason: reasonReserveCredits}, "workbuddy channel parked: reserve_credits"},
		{"cooldown 带 Until（统一 UTC）", &ParkedError{Reason: reasonCooldown, Until: cooldownUntil}, "workbuddy channel parked: cooldown until 2025-03-14T02:30:00Z"},
		{"daily_limit 带 Until", &ParkedError{Reason: reasonDailyLimit, Until: time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC)}, "workbuddy channel parked: daily_limit until 2025-03-15T00:00:00Z"},
		{"nil 接收者", nil, "workbuddy channel parked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParkedErrorDisabledHasZeroUntil(t *testing.T) {
	pe := &ParkedError{Reason: reasonDisabled}
	if !pe.Until.IsZero() {
		t.Fatalf("disabled Until = %v, want zero", pe.Until)
	}
}

// ---- admitDecision 表驱动 ----

func TestAdmitDecision(t *testing.T) {
	now := stateTestNow
	enabled := &model.WorkbuddySetting{Enabled: true}
	observed := now.Add(-time.Hour)

	cases := []struct {
		name       string
		settings   *model.WorkbuddySetting
		state      *model.WorkbuddyChannelState
		upModel    string
		today      func() (int64, bool)
		wantReason string
		wantUntil  time.Time
	}{
		{
			name: "总开关关闭", settings: &model.WorkbuddySetting{Enabled: false},
			state: &model.WorkbuddyChannelState{Credits: 100}, upModel: "m",
			wantReason: reasonDisabled,
		},
		{
			name: "settings 为 nil 视为关闭", settings: nil,
			state: &model.WorkbuddyChannelState{Credits: 100}, upModel: "m",
			wantReason: reasonDisabled,
		},
		{
			name: "运行态缺失放行", settings: enabled,
			state: nil, upModel: "m",
		},
		{
			name: "冷却未到期", settings: enabled,
			state:   &model.WorkbuddyChannelState{Cooldowns: map[string]time.Time{"m": now.Add(5 * time.Minute)}},
			upModel: "m", wantReason: reasonCooldown, wantUntil: now.Add(5 * time.Minute),
		},
		{
			name: "冷却已过", settings: enabled,
			state:   &model.WorkbuddyChannelState{Cooldowns: map[string]time.Time{"m": now.Add(-time.Minute)}},
			upModel: "m",
		},
		{
			name: "冷却只对命中的模型生效", settings: enabled,
			state:   &model.WorkbuddyChannelState{Cooldowns: map[string]time.Time{"other": now.Add(5 * time.Minute)}},
			upModel: "m",
		},
		{
			name: "保留积分等于阈值触发", settings: &model.WorkbuddySetting{Enabled: true, ReserveCredits: 10},
			state:   &model.WorkbuddyChannelState{Credits: 10, CreditsAt: observed},
			upModel: "m", wantReason: reasonReserveCredits,
		},
		{
			name: "保留积分低于阈值触发", settings: &model.WorkbuddySetting{Enabled: true, ReserveCredits: 10},
			state:   &model.WorkbuddyChannelState{Credits: 5, CreditsAt: observed},
			upModel: "m", wantReason: reasonReserveCredits,
		},
		{
			name: "保留积分高于阈值放行", settings: &model.WorkbuddySetting{Enabled: true, ReserveCredits: 10},
			state:   &model.WorkbuddyChannelState{Credits: 50, CreditsAt: observed},
			upModel: "m",
		},
		{
			name: "积分从未观测不阻塞", settings: &model.WorkbuddySetting{Enabled: true, ReserveCredits: 10},
			state:   &model.WorkbuddyChannelState{Credits: 0},
			upModel: "m",
		},
		{
			name: "未配置保留额不判定", settings: &model.WorkbuddySetting{Enabled: true, ReserveCredits: 0},
			state:   &model.WorkbuddyChannelState{Credits: 0, CreditsAt: observed},
			upModel: "m",
		},
		{
			name: "日限额达到", settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
			state:   &model.WorkbuddyChannelState{},
			upModel: "m", today: func() (int64, bool) { return 100, true },
			wantReason: reasonDailyLimit, wantUntil: nextLocalDayStart(now),
		},
		{
			name: "日限额超过", settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
			state:   &model.WorkbuddyChannelState{},
			upModel: "m", today: func() (int64, bool) { return 101, true },
			wantReason: reasonDailyLimit, wantUntil: nextLocalDayStart(now),
		},
		{
			name: "日限额未达到", settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
			state:   &model.WorkbuddyChannelState{},
			upModel: "m", today: func() (int64, bool) { return 99, true },
		},
		{
			name: "当日无数据不阻塞", settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
			state:   &model.WorkbuddyChannelState{},
			upModel: "m", today: func() (int64, bool) { return 0, false },
		},
		{
			name: "未配置日限额不判定", settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 0},
			state:   &model.WorkbuddyChannelState{},
			upModel: "m", today: func() (int64, bool) { return 1 << 40, true },
		},
		{
			name: "todayTokens 为 nil 不阻塞", settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 1},
			state:   &model.WorkbuddyChannelState{},
			upModel: "m", today: nil,
		},
		{
			name: "冷却优先于保留积分", settings: &model.WorkbuddySetting{Enabled: true, ReserveCredits: 100},
			state: &model.WorkbuddyChannelState{
				Credits: 1, CreditsAt: observed,
				Cooldowns: map[string]time.Time{"m": now.Add(time.Minute)},
			},
			upModel: "m", wantReason: reasonCooldown, wantUntil: now.Add(time.Minute),
		},
		{
			name: "保留积分优先于日限额", settings: &model.WorkbuddySetting{Enabled: true, ReserveCredits: 100, DailyTokenLimit: 1},
			state:   &model.WorkbuddyChannelState{Credits: 1, CreditsAt: observed},
			upModel: "m", today: func() (int64, bool) { return 1 << 40, true },
			wantReason: reasonReserveCredits,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := admitDecision(tc.settings, tc.state, tc.upModel, now, tc.today)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("admitDecision() = %v, want nil", err)
				}
				return
			}
			pe := parkedOf(t, err)
			if pe.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", pe.Reason, tc.wantReason)
			}
			if !tc.wantUntil.IsZero() && !pe.Until.Equal(tc.wantUntil) {
				t.Fatalf("Until = %v, want %v", pe.Until, tc.wantUntil)
			}
			if tc.wantUntil.IsZero() && !pe.Until.IsZero() {
				t.Fatalf("Until = %v, want zero", pe.Until)
			}
		})
	}
}

// ---- 冷却时长 ----

func TestCooldownFor(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want time.Duration
	}{
		{"未提供 Retry-After 默认 60s", Event{Status: 429}, defaultCooldown},
		{"负数按未提供处理", Event{Status: 429, RetryAfter: -time.Second}, defaultCooldown},
		{"采用 Retry-After", Event{Status: 429, RetryAfter: 5 * time.Second}, 5 * time.Second},
		{"恰好 1h 不截断", Event{Status: 429, RetryAfter: time.Hour}, time.Hour},
		{"超长 Retry-After 封顶 1h", Event{Status: 429, RetryAfter: 24 * time.Hour}, maxCooldown},
		{"极端 Retry-After 仍封顶 1h", Event{Status: 429, RetryAfter: 365 * 24 * time.Hour}, maxCooldown},

		// ---- 风控 403(11140)：固定 2h，绕过 1h 上限、不采信 Retry-After ----
		// 对齐参考实现 RESTRICTED_403_COOLDOWN = 7200.0（commit be4f9f8 / PR #288）。
		{"风控 403 无 Retry-After 默认 2h", Event{Status: 403, UpCode: upstreamCodeAccountRestricted}, riskCooldown},
		{"风控 403 的 Retry-After 不改写 2h", Event{Status: 403, UpCode: upstreamCodeAccountRestricted, RetryAfter: 5 * time.Second}, riskCooldown},
		{"风控 403 的 2h 不被 1h 上限裁剪", Event{Status: 403, UpCode: upstreamCodeAccountRestricted, RetryAfter: time.Hour}, riskCooldown},
		// 同码但状态不是 403（防御）：按普通冷却处理，避免把未知语义当风控。
		{"11140 但状态 200 不算风控", Event{Status: 200, UpCode: upstreamCodeAccountRestricted}, defaultCooldown},
		// 内容审核 / 未知 403 / 未知 5xx：不走风控分支。
		{"内容审核 403 走默认冷却", Event{Status: 403, UpCode: 10001}, defaultCooldown},
		{"未知 403（code=0）走默认冷却", Event{Status: 403}, defaultCooldown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cooldownFor(tc.ev); got != tc.want {
				t.Fatalf("cooldownFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- 本地日窗口 ----

func TestLocalDayBoundary(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	early := time.Date(2025, 3, 14, 0, 0, 5, 0, cst)
	late := time.Date(2025, 3, 14, 23, 59, 59, 0, cst)
	if localDay(early) != "2025-03-14" || localDay(late) != "2025-03-14" {
		t.Fatalf("同一天应得到相同日期串: %q / %q", localDay(early), localDay(late))
	}
	if localDay(early) != localDay(late) {
		t.Fatal("00:00:05 与 23:59:59 必须落在同一本地日")
	}

	// 次日零点边界。
	if got, want := nextLocalDayStart(late), time.Date(2025, 3, 15, 0, 0, 0, 0, cst); !got.Equal(want) {
		t.Fatalf("nextLocalDayStart = %v, want %v", got, want)
	}
	// 跨月。
	dec := time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC)
	if got, want := nextLocalDayStart(dec), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("跨年 nextLocalDayStart = %v, want %v", got, want)
	}
	if localDay(dec) != "2025-12-31" {
		t.Fatalf("跨年日期串 = %q", localDay(dec))
	}
	// 闰年 2 月。
	feb := time.Date(2024, 2, 28, 12, 0, 0, 0, time.UTC)
	if got, want := nextLocalDayStart(feb), time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("闰年 nextLocalDayStart = %v, want %v", got, want)
	}
	// 平年 2 月。
	feb = time.Date(2025, 2, 28, 12, 0, 0, 0, time.UTC)
	if got, want := nextLocalDayStart(feb), time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("平年 nextLocalDayStart = %v, want %v", got, want)
	}
	// 当地日与 UTC 日不同（跨时区不串日）。
	tokyo := time.FixedZone("JST", 9*3600)
	instant := time.Date(2025, 3, 14, 20, 0, 0, 0, time.UTC) // JST 已是 3/15
	if localDay(instant.In(tokyo)) != "2025-03-15" || localDay(instant.UTC()) != "2025-03-14" {
		t.Fatalf("时区串日处理错误: jst=%q utc=%q", localDay(instant.In(tokyo)), localDay(instant.UTC()))
	}
}

// ---- Redis 键格式 ----

func TestRedisKeyFormats(t *testing.T) {
	if got, want := cooldownKey("workbuddy", 7, "claude-3-5-sonnet"), "workbuddy:cd:7:claude-3-5-sonnet"; got != want {
		t.Fatalf("cooldownKey = %q, want %q", got, want)
	}
	// 渠道级风控冷却用通配键。
	if got, want := cooldownKey("workbuddy", 7, allModelsGlob), "workbuddy:cd:7:*"; got != want {
		t.Fatalf("渠道级 cooldownKey = %q, want %q", got, want)
	}
	if got, want := tokensKey("workbuddy", 7, "2025-03-14"), "workbuddy:tokens:7:2025-03-14"; got != want {
		t.Fatalf("tokensKey = %q, want %q", got, want)
	}
	if got, want := creditsKey("workbuddy", 7), "workbuddy:credits:7"; got != want {
		t.Fatalf("creditsKey = %q, want %q", got, want)
	}
}

// ---- nil / 非本包渠道 no-op ----

func TestAdmitObserveNilSafety(t *testing.T) {
	var nilRT *Runtime
	wb := stateTestProvider()

	if err := nilRT.Admit(nil, "m"); err != nil {
		t.Fatalf("nil Runtime.Admit = %v, want nil", err)
	}
	if err := nilRT.Admit(wb, "m"); err != nil {
		t.Fatalf("nil Runtime.Admit(wb) = %v, want nil", err)
	}
	nilRT.Observe(Event{})                            // 不得 panic
	nilRT.Observe(Event{Provider: wb, Success: true}) // 不得 panic
	nilRT.Cooldowns(wb)                               // 不得 panic
	if _, err := nilRT.ProbeCredits(context.Background(), nil, wb, nil); err == nil {
		t.Fatal("nil Runtime.ProbeCredits 应返回错误")
	}

	zero := &Runtime{}
	if err := zero.Admit(nil, "m"); err != nil {
		t.Fatalf("零值 Runtime.Admit(nil) = %v, want nil", err)
	}
	if err := zero.Admit(wb, "m"); err != nil {
		t.Fatalf("零值 Runtime（Op=nil）.Admit = %v, want nil", err)
	}
	zero.Observe(Event{Provider: wb, Success: true}) // 不得 panic
	if got := zero.Cooldowns(wb); got != nil {
		t.Fatalf("零值 Runtime.Cooldowns = %v, want nil", got)
	}
}

func TestAdmitObserveNonWorkBuddyNoop(t *testing.T) {
	db := &fakeDB{settings: &model.WorkbuddySetting{Enabled: true}}
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	rt := newTestRuntime(db, cs, stateTestNow)
	nonWB := &model.Provider{ID: 1, UserID: 2, OAuthProvider: "kimi"}

	if err := rt.Admit(nonWB, "m"); err != nil {
		t.Fatalf("非 workbuddy Admit = %v, want nil", err)
	}
	if err := rt.Admit(nil, "m"); err != nil {
		t.Fatalf("nil provider Admit = %v, want nil", err)
	}
	rt.Observe(Event{Provider: nonWB, Success: true, Tokens: 100})
	rt.Observe(Event{Provider: nil, Success: true})

	if db.settingCalls != 0 || db.stateCalls != 0 {
		t.Fatalf("非 workbuddy 渠道不应触碰状态存储: settings=%d state=%d", db.settingCalls, db.stateCalls)
	}
	if len(db.upserts) != 0 || len(db.daily) != 0 {
		t.Fatalf("非 workbuddy 渠道不应落库: upserts=%d daily=%d", len(db.upserts), len(db.daily))
	}
	if got := rt.Cooldowns(nonWB); got != nil {
		t.Fatalf("非 workbuddy Cooldowns = %v, want nil", got)
	}
}

// ---- Admit 全链路（含 Redis 分支） ----

func TestAdmitFullDisabled(t *testing.T) {
	db := &fakeDB{settings: &model.WorkbuddySetting{Enabled: false}}
	rt := newTestRuntime(db, newFakeCounter(nil), stateTestNow)
	pe := parkedOf(t, rt.Admit(stateTestProvider(), "m"))
	if pe.Reason != reasonDisabled {
		t.Fatalf("Reason = %q, want disabled", pe.Reason)
	}
}

func TestAdmitFullStateMissingAllows(t *testing.T) {
	db := &fakeDB{settings: &model.WorkbuddySetting{Enabled: true}}
	rt := newTestRuntime(db, newFakeCounter(nil), stateTestNow)
	if err := rt.Admit(stateTestProvider(), "m"); err != nil {
		t.Fatalf("运行态缺行应放行, got %v", err)
	}
}

func TestAdmitFullFailsOpenOnStorageError(t *testing.T) {
	db := &fakeDB{settingErr: errors.New("db down")}
	rt := newTestRuntime(db, newFakeCounter(nil), stateTestNow)
	if err := rt.Admit(stateTestProvider(), "m"); err != nil {
		t.Fatalf("设置读失败应 fail-open, got %v", err)
	}

	db2 := &fakeDB{settings: &model.WorkbuddySetting{Enabled: true}, stateErr: errors.New("db down")}
	rt2 := newTestRuntime(db2, newFakeCounter(nil), stateTestNow)
	if err := rt2.Admit(stateTestProvider(), "m"); err != nil {
		t.Fatalf("运行态读失败应 fail-open, got %v", err)
	}
}

func TestAdmitFullCooldownFromRedis(t *testing.T) {
	p := stateTestProvider()
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	until := stateTestNow.Add(5 * time.Minute)
	cs.Set(context.Background(), cooldownKey("workbuddy", p.ID, "m"), until.UTC().Format(time.RFC3339Nano), 5*time.Minute)

	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	rt := newTestRuntime(db, cs, stateTestNow)
	pe := parkedOf(t, rt.Admit(p, "m"))
	if pe.Reason != reasonCooldown || !pe.Until.Equal(until) {
		t.Fatalf("Redis 冷却未命中: reason=%q until=%v", pe.Reason, pe.Until)
	}
}

func TestAdmitFullCooldownFallsBackToPostgres(t *testing.T) {
	p := stateTestProvider()
	until := stateTestNow.Add(3 * time.Minute)
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state: &model.WorkbuddyChannelState{
			UserID: p.UserID, ProviderID: p.ID,
			Cooldowns: map[string]time.Time{"m": until},
		},
	}
	// 无 Redis（单实例部署）：回落运行态 map。
	rt := newTestRuntime(db, nil, stateTestNow)
	pe := parkedOf(t, rt.Admit(p, "m"))
	if pe.Reason != reasonCooldown || !pe.Until.Equal(until) {
		t.Fatalf("Postgres 冷却未命中: reason=%q until=%v", pe.Reason, pe.Until)
	}
}

func TestAdmitFullDailyTokenFromRedis(t *testing.T) {
	p := stateTestProvider()
	today := localDay(stateTestNow)
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	cs.Set(context.Background(), tokensKey("workbuddy", p.ID, today), "150", time.Hour)

	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	rt := newTestRuntime(db, cs, stateTestNow)
	pe := parkedOf(t, rt.Admit(p, "m"))
	if pe.Reason != reasonDailyLimit {
		t.Fatalf("Redis 当日计数未命中: reason=%q", pe.Reason)
	}
	if want := nextLocalDayStart(stateTestNow); !pe.Until.Equal(want) {
		t.Fatalf("Until = %v, want %v", pe.Until, want)
	}
}

func TestAdmitFullDailyTokenUsesStateCacheThenAggregate(t *testing.T) {
	p := stateTestProvider()
	today := localDay(stateTestNow)

	// 运行态当日缓存命中：不得走聚合。
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
		state: &model.WorkbuddyChannelState{
			UserID: p.UserID, ProviderID: p.ID,
			DailyTokens: 120, TokensDate: today,
		},
		sum: 0,
	}
	rt := newTestRuntime(db, newFakeCounter(func() time.Time { return stateTestNow }), stateTestNow)
	if pe := parkedOf(t, rt.Admit(p, "m")); pe.Reason != reasonDailyLimit {
		t.Fatalf("运行态当日缓存未命中: reason=%q", pe.Reason)
	}

	// 运行态是昨天：回落 usage_logs 聚合。
	db2 := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
		state: &model.WorkbuddyChannelState{
			UserID: p.UserID, ProviderID: p.ID,
			DailyTokens: 120, TokensDate: "2025-03-13",
		},
		sum: 5,
	}
	rt2 := newTestRuntime(db2, newFakeCounter(func() time.Time { return stateTestNow }), stateTestNow)
	if err := rt2.Admit(p, "m"); err != nil {
		t.Fatalf("跨天聚合应放行（聚合值 5 < 100）, got %v", err)
	}
}

func TestAdmitFullUncountedDayDoesNotPark(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true, DailyTokenLimit: 100},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	db.sumErr = errors.New("aggregate failed")
	rt := newTestRuntime(db, newFakeCounter(func() time.Time { return stateTestNow }), stateTestNow)
	if err := rt.Admit(p, "m"); err != nil {
		t.Fatalf("无数据日不得阻塞, got %v", err)
	}
}

// ---- Observe：成功分支 ----

func TestObserveSuccessClearsCooldownAndAccumulates(t *testing.T) {
	p := stateTestProvider()
	today := localDay(stateTestNow)
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	cs.Set(context.Background(), cooldownKey("workbuddy", p.ID, "m"), stateTestNow.Add(time.Hour).UTC().Format(time.RFC3339Nano), time.Hour)
	cs.Set(context.Background(), tokensKey("workbuddy", p.ID, today), "100", time.Hour)

	db := &fakeDB{state: &model.WorkbuddyChannelState{
		UserID: p.UserID, ProviderID: p.ID,
		DailyTokens: 100, TokensDate: today,
		Cooldowns: map[string]time.Time{
			"m":   stateTestNow.Add(10 * time.Minute),
			"old": stateTestNow.Add(-time.Hour),
		},
	}}
	rt := newTestRuntime(db, cs, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Success: true, Tokens: 20})

	if _, ok := cs.Get(context.Background(), cooldownKey("workbuddy", p.ID, "m")); ok {
		t.Fatal("成功应删除 Redis 冷却键")
	}
	if len(db.upserts) != 1 {
		t.Fatalf("应写一次 last_ok, upserts=%d", len(db.upserts))
	}
	st := db.state
	if _, ok := st.Cooldowns["m"]; ok {
		t.Fatal("成功应删除该模型冷却")
	}
	if _, ok := st.Cooldowns["old"]; ok {
		t.Fatal("过期冷却应被清理")
	}
	if st.LastOKAt == nil || !st.LastOKAt.Equal(stateTestNow) {
		t.Fatalf("LastOKAt = %v, want %v", st.LastOKAt, stateTestNow)
	}
	if len(db.daily) != 1 || db.daily[0].date != today || db.daily[0].delta != 20 {
		t.Fatalf("当日用量累加错误: %+v", db.daily)
	}
	raw, ok := cs.Get(context.Background(), tokensKey("workbuddy", p.ID, today))
	if !ok || raw != "120" {
		t.Fatalf("Redis 当日计数 = %q（ok=%v）, want 120", raw, ok)
	}
	wantTTL := nextLocalDayStart(stateTestNow).Add(time.Hour).Sub(stateTestNow)
	if got := cs.ttl(tokensKey("workbuddy", p.ID, today)); got != wantTTL {
		t.Fatalf("当日计数 TTL = %v, want %v", got, wantTTL)
	}
}

func TestObserveSuccessWithoutRedisStillWritesPostgres(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{state: &model.WorkbuddyChannelState{
		UserID: p.UserID, ProviderID: p.ID,
		Cooldowns: map[string]time.Time{"m": stateTestNow.Add(time.Minute)},
	}}
	rt := newTestRuntime(db, nil, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Success: true, Tokens: 5})

	if len(db.upserts) != 1 {
		t.Fatalf("无 Redis 也应写 last_ok, upserts=%d", len(db.upserts))
	}
	if db.state.LastOKAt == nil {
		t.Fatal("无 Redis 应写 LastOKAt")
	}
	if _, ok := db.state.Cooldowns["m"]; ok {
		t.Fatal("无 Redis 也应清 Postgres 冷却")
	}
	if len(db.daily) != 1 || db.daily[0].delta != 5 {
		t.Fatalf("无 Redis 也应累加当日用量: %+v", db.daily)
	}
}

func TestObserveSuccessNoTokensSkipsDaily(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{state: &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID}}
	rt := newTestRuntime(db, newFakeCounter(nil), stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Success: true, Tokens: 0})

	if len(db.daily) != 0 {
		t.Fatalf("Tokens=0 不应累加当日用量: %+v", db.daily)
	}
	if len(db.upserts) != 1 {
		t.Fatalf("仍应刷新 last_ok, upserts=%d", len(db.upserts))
	}
}

// ---- Observe：冷却分支 ----

func TestObserveCooldownWritesRedisAndPostgres(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{state: &model.WorkbuddyChannelState{
		UserID: p.UserID, ProviderID: p.ID,
		Cooldowns: map[string]time.Time{"old": stateTestNow.Add(-time.Minute)},
	}}
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	rt := newTestRuntime(db, cs, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Status: 429, RetryAfter: 5 * time.Second})

	key := cooldownKey("workbuddy", p.ID, "m")
	raw, ok := cs.Get(context.Background(), key)
	if !ok {
		t.Fatal("429 应写 Redis 冷却键")
	}
	until, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("冷却值不可解析: %v", err)
	}
	if want := stateTestNow.Add(5 * time.Second); !until.Equal(want) {
		t.Fatalf("冷却截止 = %v, want %v", until, want)
	}
	if got := cs.ttl(key); got != 5*time.Second {
		t.Fatalf("冷却 TTL = %v, want 5s", got)
	}
	if len(db.upserts) != 1 {
		t.Fatalf("429 应写 Postgres, upserts=%d", len(db.upserts))
	}
	if _, ok := db.state.Cooldowns["old"]; ok {
		t.Fatal("过期冷却应被裁剪")
	}
	if got := db.state.Cooldowns["m"]; !got.Equal(stateTestNow.Add(5 * time.Second)) {
		t.Fatalf("Postgres 冷却 = %v", got)
	}
}

func TestObserveCooldownCapsHugeRetryAfter(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	rt := newTestRuntime(db, cs, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Status: 429, RetryAfter: 24 * time.Hour})

	if got := cs.ttl(cooldownKey("workbuddy", p.ID, "m")); got != maxCooldown {
		t.Fatalf("超长 Retry-After 应封顶 1h, TTL=%v", got)
	}
	if got := db.state.Cooldowns["m"]; !got.Equal(stateTestNow.Add(maxCooldown)) {
		t.Fatalf("Postgres 冷却应封顶 1h, got %v", got)
	}
}

func TestObserveCooldownDefaultsAndUpstreamCode(t *testing.T) {
	p := stateTestProvider()

	// Retry-After 缺失 → 默认 60s。
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	rt := newTestRuntime(db, cs, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Status: 429})
	if got := cs.ttl(cooldownKey("workbuddy", p.ID, "m")); got != defaultCooldown {
		t.Fatalf("默认冷却 = %v, want 60s", got)
	}

	// 上游业务码 6004（限额），HTTP 非 429 也触发。
	db2 := &fakeDB{state: &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID}}
	cs2 := newFakeCounter(func() time.Time { return stateTestNow })
	rt2 := newTestRuntime(db2, cs2, stateTestNow)
	rt2.Observe(Event{Provider: p, UpModel: "m", Status: 200, UpCode: upstreamCodeDailyLimit, RetryAfter: 30 * time.Second})
	if got := cs2.ttl(cooldownKey("workbuddy", p.ID, "m")); got != 30*time.Second {
		t.Fatalf("6004 冷却 = %v, want 30s", got)
	}
	if _, ok := db2.state.Cooldowns["m"]; !ok {
		t.Fatal("6004 应写 Postgres 冷却")
	}
}

// TestObserve429WithoutHintStaysModelScoped 钉住 429 的作用域映射（参考实现
// wb_accounts.py:414-450 / wb_proxy.py:11531）：没有 Retry-After 的 429 走
// 模型级 60s，不写渠道级通配键 —— 参考实现只在正文声明凭证级时才给账号
// 600s→2h 阶梯，且它专门修掉了「裸 429 冷却整个账号」的故障
// （wb_proxy.py:11263 rate_limit_is_account_level docstring）；我们的 Event
// 拿不到正文作用域，因此保持模型级默认，不做 600s 猜测。
func TestObserve429WithoutHintStaysModelScoped(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	rt := newTestRuntime(db, cs, stateTestNow)

	rt.Observe(Event{Provider: p, UpModel: "m", Status: http.StatusTooManyRequests})

	if got := cs.ttl(cooldownKey("workbuddy", p.ID, "m")); got != defaultCooldown {
		t.Fatalf("无提示 429 应为模型级 60s, ttl=%v", got)
	}
	if got := cs.ttl(cooldownKey("workbuddy", p.ID, allModelsGlob)); got != 0 {
		t.Fatalf("无提示 429 不应冷却整个渠道, 通配 ttl=%v", got)
	}
	// 别的模型照常可用（模型级窗口的语义）。
	if err := rt.Admit(p, "other"); err != nil {
		t.Fatalf("无提示 429 不应牵连其它模型, got %v", err)
	}
}

func TestObserveCooldownWithoutRedis(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{state: &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID}}
	rt := newTestRuntime(db, nil, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Status: 429, RetryAfter: time.Minute})

	if len(db.upserts) != 1 {
		t.Fatalf("无 Redis 也应写 Postgres, upserts=%d", len(db.upserts))
	}
	if got := db.state.Cooldowns["m"]; !got.Equal(stateTestNow.Add(time.Minute)) {
		t.Fatalf("Postgres 冷却 = %v", got)
	}
}

func TestObserveUnrelatedFailureDoesNothing(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	rt := newTestRuntime(db, cs, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Status: 500})

	if len(db.upserts) != 0 || cs.ttl(cooldownKey("workbuddy", p.ID, "m")) != 0 {
		t.Fatal("非成功/非 429-6004 不应产生任何写")
	}
}

// ---- Observe：风控 403(11140) 与内容审核 403 分流 ----

// TestObserveRiskControl403CoolsWholeChannel 钉住 be4f9f8 / PR #288 的行为：
// 上游 403 + code 11140（账号存在异常行为，已被限制使用）是账号级的坏，
// 冷却整条渠道 2h（通配键，对所有模型生效），而不是只停当前模型。
func TestObserveRiskControl403CoolsWholeChannel(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	rt := newTestRuntime(db, cs, stateTestNow)

	rt.Observe(Event{Provider: p, UpModel: "m", Status: http.StatusForbidden, UpCode: upstreamCodeAccountRestricted})

	wantUntil := stateTestNow.Add(riskCooldown)
	// Redis：整条渠道的通配冷却键，TTL = 2h。
	key := cooldownKey("workbuddy", p.ID, allModelsGlob)
	raw, ok := cs.Get(context.Background(), key)
	if !ok {
		t.Fatalf("风控 403 应写通配 Redis 冷却键 %s", key)
	}
	until, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !until.Equal(wantUntil) {
		t.Fatalf("通配冷却截止 = %v（err=%v）, want %v", until, err, wantUntil)
	}
	if got := cs.ttl(key); got != riskCooldown {
		t.Fatalf("通配冷却 TTL = %v, want %v", got, riskCooldown)
	}
	// 不写模型级键：风控是账号级，模型级键会给「别的模型照用」留口子。
	if _, ok := cs.Get(context.Background(), cooldownKey("workbuddy", p.ID, "m")); ok {
		t.Fatal("风控 403 不应写模型级冷却键")
	}
	// Postgres：同样只写通配键。
	if got, ok := db.state.Cooldowns[allModelsGlob]; !ok || !got.Equal(wantUntil) {
		t.Fatalf("Postgres 通配冷却 = %v（ok=%v）, want %v", got, ok, wantUntil)
	}
	if _, ok := db.state.Cooldowns["m"]; ok {
		t.Fatal("Postgres 不应写模型级冷却")
	}
	if len(db.upserts) != 1 {
		t.Fatalf("应写一次运行态, upserts=%d", len(db.upserts))
	}

	// 渠道级生效：请求任何模型都被准入拦下，并带 2h 恢复时间（客户端可退避）。
	for _, upModel := range []string{"m", "another-model"} {
		pe := parkedOf(t, rt.Admit(p, upModel))
		if pe.Reason != reasonCooldown {
			t.Fatalf("模型 %q: Reason = %q, want cooldown", upModel, pe.Reason)
		}
		if !pe.Until.Equal(wantUntil) {
			t.Fatalf("模型 %q: Until = %v, want %v", upModel, pe.Until, wantUntil)
		}
	}
}

// TestObserveContentModeration403NoCooldown 钉住分流：内容审核 403（如 code
// 10001）与未知 403 保持原有行为 —— 不给账号上长冷却（这不是账号的坏），
// 只照常 failover 换号。
func TestObserveContentModeration403NoCooldown(t *testing.T) {
	p := stateTestProvider()
	for _, tc := range []struct {
		name   string
		upCode int
	}{
		{"内容审核 10001", 10001},
		{"未知 403（code=0）", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeDB{
				settings: &model.WorkbuddySetting{Enabled: true},
				state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
			}
			cs := newFakeCounter(func() time.Time { return stateTestNow })
			rt := newTestRuntime(db, cs, stateTestNow)

			rt.Observe(Event{Provider: p, UpModel: "m", Status: http.StatusForbidden, UpCode: tc.upCode})

			if len(db.upserts) != 0 {
				t.Fatalf("不应写运行态, upserts=%d", len(db.upserts))
			}
			if len(db.state.Cooldowns) != 0 {
				t.Fatalf("不应写冷却, cooldowns=%v", db.state.Cooldowns)
			}
			if got := cs.ttl(cooldownKey("workbuddy", p.ID, allModelsGlob)); got != 0 {
				t.Fatalf("不应写通配冷却键, ttl=%v", got)
			}
			if got := cs.ttl(cooldownKey("workbuddy", p.ID, "m")); got != 0 {
				t.Fatalf("不应写模型冷却键, ttl=%v", got)
			}
			if err := rt.Admit(p, "m"); err != nil {
				t.Fatalf("内容审核/未知 403 不应准入拦截, got %v", err)
			}
		})
	}
}

// TestObserveUnknown5xxTransientNoCooldown 钉住 PR #288 的另一半：一切 >=500
// （含上游自定义的 550）按瞬时故障处理，不写账号冷却；换号由 relay 的 failover
// 负责（任何 callErr / status>=500 都会 continue 到下一候选，见 relay/route.go
// shouldFailover），所以 workbuddy 侧不加冗余冷却。
func TestObserveUnknown5xxTransientNoCooldown(t *testing.T) {
	p := stateTestProvider()
	for _, status := range []int{500, 502, 503, 504, 550} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			db := &fakeDB{
				settings: &model.WorkbuddySetting{Enabled: true},
				state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
			}
			cs := newFakeCounter(func() time.Time { return stateTestNow })
			rt := newTestRuntime(db, cs, stateTestNow)

			rt.Observe(Event{Provider: p, UpModel: "m", Status: status})

			if len(db.upserts) != 0 || len(db.state.Cooldowns) != 0 {
				t.Fatalf("5xx %d 不应写冷却: upserts=%d cooldowns=%v", status, len(db.upserts), db.state.Cooldowns)
			}
			if got := cs.ttl(cooldownKey("workbuddy", p.ID, allModelsGlob)); got != 0 {
				t.Fatalf("5xx %d 不应写通配冷却键, ttl=%v", status, got)
			}
			if err := rt.Admit(p, "m"); err != nil {
				t.Fatalf("5xx %d 不应准入拦截, got %v", status, err)
			}
		})
	}
}

// TestAdmitChannelWideCooldownFromRedis 渠道级冷却只存在于 Redis（Postgres 行
// 尚未回写）时也必须生效 —— 转发热路径优先读 Redis。
func TestAdmitChannelWideCooldownFromRedis(t *testing.T) {
	p := stateTestProvider()
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	until := stateTestNow.Add(90 * time.Minute)
	cs.Set(context.Background(), cooldownKey("workbuddy", p.ID, allModelsGlob),
		until.UTC().Format(time.RFC3339Nano), 90*time.Minute)

	db := &fakeDB{
		settings: &model.WorkbuddySetting{Enabled: true},
		state:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	rt := newTestRuntime(db, cs, stateTestNow)
	pe := parkedOf(t, rt.Admit(p, "any-model"))
	if pe.Reason != reasonCooldown || !pe.Until.Equal(until) {
		t.Fatalf("Redis 渠道级冷却未命中: reason=%q until=%v", pe.Reason, pe.Until)
	}
}

// TestAdmitCooldownTakesLaterOfChannelAndModel 渠道级与模型级冷却并存时取较晚者：
// 一条更短的模型冷却不能把 2h 的风控冷却「盖短」，反之亦然。
func TestAdmitCooldownTakesLaterOfChannelAndModel(t *testing.T) {
	p := stateTestProvider()
	cases := []struct {
		name    string
		channel time.Time
		model   time.Time
		want    time.Time
	}{
		{"渠道更晚取渠道", stateTestNow.Add(riskCooldown), stateTestNow.Add(5 * time.Minute), stateTestNow.Add(riskCooldown)},
		{"模型更晚取模型", stateTestNow.Add(time.Hour), stateTestNow.Add(3 * time.Hour), stateTestNow.Add(3 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeDB{
				settings: &model.WorkbuddySetting{Enabled: true},
				state: &model.WorkbuddyChannelState{
					UserID: p.UserID, ProviderID: p.ID,
					Cooldowns: map[string]time.Time{allModelsGlob: tc.channel, "m": tc.model},
				},
			}
			rt := newTestRuntime(db, nil, stateTestNow)
			pe := parkedOf(t, rt.Admit(p, "m"))
			if pe.Reason != reasonCooldown || !pe.Until.Equal(tc.want) {
				t.Fatalf("Until = %v（reason=%q）, want %v", pe.Until, pe.Reason, tc.want)
			}
		})
	}
}

// ---- Cooldowns 合并 ----

func TestCooldownsMergesRedisAndPrunesExpired(t *testing.T) {
	p := stateTestProvider()
	cs := newFakeCounter(func() time.Time { return stateTestNow })
	fresh := stateTestNow.Add(20 * time.Minute)
	cs.Set(context.Background(), cooldownKey("workbuddy", p.ID, "m"), fresh.UTC().Format(time.RFC3339Nano), 20*time.Minute)

	db := &fakeDB{state: &model.WorkbuddyChannelState{
		UserID: p.UserID, ProviderID: p.ID,
		Cooldowns: map[string]time.Time{
			"m":    stateTestNow.Add(10 * time.Minute), // Redis 更新 → 覆盖
			"gone": stateTestNow.Add(-time.Minute),     // 过期 → 剔除
		},
	}}
	rt := newTestRuntime(db, cs, stateTestNow)
	got := rt.Cooldowns(p)
	if len(got) != 1 {
		t.Fatalf("Cooldowns = %v, want 仅 m", got)
	}
	if until, ok := got["m"]; !ok || !until.Equal(fresh) {
		t.Fatalf("m 冷却 = %v, want %v", until, fresh)
	}

	// 全部过期 → nil。
	db2 := &fakeDB{state: &model.WorkbuddyChannelState{
		UserID: p.UserID, ProviderID: p.ID,
		Cooldowns: map[string]time.Time{"gone": stateTestNow.Add(-time.Minute)},
	}}
	rt2 := newTestRuntime(db2, nil, stateTestNow)
	if got := rt2.Cooldowns(p); got != nil {
		t.Fatalf("全过期应返回 nil, got %v", got)
	}
}

// ---- ProbeCredits ----

func newProbeRuntime(t *testing.T, srv *httptest.Server, db stateStore, cs counterStore) *Runtime {
	t.Helper()
	rt := newTestRuntime(db, cs, stateTestNow)
	rt.creditsURL = func(realm Realm) string {
		if realm != RealmIntl {
			t.Fatalf("探测区域 = %q, want intl", realm)
		}
		return srv.URL
	}
	return rt
}

// probeServer 起一个校验请求形状、返回固定响应的 httptest 服务。
func probeServer(t *testing.T, status int, body string, secret string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("方法 = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Domain"); got == "" {
			t.Error("缺少身分头 X-Domain")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != probeCreditsBody {
			t.Errorf("请求体 = %s, want %s", b, probeCreditsBody)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func probeToken() *oauth.TokenSet {
	return &oauth.TokenSet{AccessToken: "super-secret", Extra: map[string]string{"uid": "u-1"}}
}

func TestProbeCreditsSuccessFieldVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want float64
	}{
		{"data.remaining 数字", `{"code":0,"data":{"remaining":12.5}}`, 12.5},
		{"data.balance 数字", `{"code":0,"data":{"balance":88.25}}`, 88.25},
		{"data.remaining 数字字符串", `{"code":0,"data":{"remaining":"100.75"}}`, 100.75},
		{"嵌套 data.credits", `{"code":0,"msg":"ok","data":{"credits":7,"other":"x"}}`, 7},
		{"顶层 available 回落", `{"code":0,"available":9}`, 9},
		{"字段名大小写不敏感", `{"code":0,"DATA":{"Remaining":3}}`, 3},
		{"优先级 remaining 高于 balance", `{"data":{"balance":1,"remaining":2}}`, 2},
		// 上游聚合形状（wb_accounts.py:1792 把各积分包 remain 求和写入 credits）：
		// remain 是唯一数值，解析结果就是该渠道可用于判定的积分。
		{"上游聚合形状 remain", `{"code":0,"data":{"remain":42.5,"used":7.5,"size":50}}`, 42.5},
		// 「可用」维度：若真实探测响应同时给出总数与可用额，用能真正花出去的
		// available 判 reserve_credits（见 creditValueKeys 注释）。
		{"data.available", `{"code":0,"data":{"available":20}}`, 20},
		{"available 优先于 totalRemaining", `{"data":{"totalRemaining":100,"available":30}}`, 30},
		{"只有 totalRemaining 时回落", `{"data":{"totalRemaining":100}}`, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := probeServer(t, http.StatusOK, tc.body, "super-secret")
			defer srv.Close()

			db := &fakeDB{}
			cs := newFakeCounter(func() time.Time { return stateTestNow })
			rt := newProbeRuntime(t, srv, db, cs)

			got, err := rt.ProbeCredits(context.Background(), srv.Client(), stateTestProvider(), probeToken())
			if err != nil {
				t.Fatalf("ProbeCredits = %v", err)
			}
			if got != tc.want {
				t.Fatalf("credits = %v, want %v", got, tc.want)
			}
			if len(db.credits) != 1 || db.credits[0].credits != tc.want {
				t.Fatalf("未持久化积分: %+v", db.credits)
			}
			if !db.credits[0].at.Equal(stateTestNow) {
				t.Fatalf("持久化时间 = %v, want %v", db.credits[0].at, stateTestNow)
			}
			raw, ok := cs.Get(context.Background(), creditsKey("workbuddy", stateTestProvider().ID))
			if !ok {
				t.Fatal("未写 Redis 积分缓存")
			}
			if want := strconv.FormatFloat(tc.want, 'f', -1, 64); want != raw {
				t.Fatalf("积分缓存 = %q, want %q", raw, want)
			}
			if ttl := cs.ttl(creditsKey("workbuddy", stateTestProvider().ID)); ttl != creditsCacheTTL {
				t.Fatalf("积分缓存 TTL = %v, want %v", ttl, creditsCacheTTL)
			}
		})
	}
}

// TestProbeCreditsAvailableDrivesReserveDecision 把「可用值」语义接到判定上：
// 上游单值形状下解析结果就是该渠道可用的积分；若响应同时给出总数与可用额，
// 用可用额判定 reserve_credits —— 用含不可用份额的总数会误放行已耗尽的渠道。
func TestProbeCreditsAvailableDrivesReserveDecision(t *testing.T) {
	srv := probeServer(t, http.StatusOK,
		`{"code":0,"data":{"totalRemaining":100,"available":30}}`, "super-secret")
	defer srv.Close()

	db := &fakeDB{}
	rt := newProbeRuntime(t, srv, db, newFakeCounter(func() time.Time { return stateTestNow }))
	got, err := rt.ProbeCredits(context.Background(), srv.Client(), stateTestProvider(), probeToken())
	if err != nil {
		t.Fatalf("ProbeCredits = %v", err)
	}
	if got != 30 {
		t.Fatalf("credits = %v, want 可用值 30（而非总数 100）", got)
	}
	if len(db.credits) != 1 || db.credits[0].credits != 30 {
		t.Fatalf("落库积分 = %+v, want 30", db.credits)
	}

	observed := stateTestNow.Add(-time.Minute)
	state := &model.WorkbuddyChannelState{Credits: got, CreditsAt: observed}
	// 可用 30 <= 保留 50 → 拦（若误用总数 100 会放行）。
	if pe := parkedOf(t, admitDecision(&model.WorkbuddySetting{Enabled: true, ReserveCredits: 50}, state, "m", stateTestNow, nil)); pe.Reason != reasonReserveCredits {
		t.Fatalf("可用 30 应触发 reserve_credits, got reason=%q", pe.Reason)
	}
	if err := admitDecision(&model.WorkbuddySetting{Enabled: true, ReserveCredits: 10}, state, "m", stateTestNow, nil); err != nil {
		t.Fatalf("可用 30 高于保留 10 应放行, got %v", err)
	}
}

func TestProbeCreditsNon2xxStructuredError(t *testing.T) {
	// 正文里带一个「疑似泄露值」，断言它绝不进入错误串。
	srv := probeServer(t, http.StatusInternalServerError,
		`{"error":{"message":"upstream boom","code":6004},"echo":"leaked-secret-value"}`, "super-secret")
	defer srv.Close()

	db := &fakeDB{}
	rt := newProbeRuntime(t, srv, db, newFakeCounter(nil))

	credits, err := rt.ProbeCredits(context.Background(), srv.Client(), stateTestProvider(), probeToken())
	if err == nil {
		t.Fatal("非 2xx 应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Fatalf("错误应含状态码: %q", msg)
	}
	if !strings.Contains(msg, "6004") {
		t.Fatalf("错误应含上游业务码: %q", msg)
	}
	if strings.Contains(msg, "super-secret") {
		t.Fatalf("错误串泄露了 access token: %q", msg)
	}
	if strings.Contains(msg, "leaked-secret-value") {
		t.Fatalf("错误串回显了正文值: %q", msg)
	}
	if credits != 0 {
		t.Fatalf("失败时 credits = %v, want 0", credits)
	}
	if len(db.credits) != 0 {
		t.Fatal("失败不应持久化积分")
	}
}

func TestProbeCreditsNonJSONBodyNotEchoed(t *testing.T) {
	srv := probeServer(t, http.StatusBadGateway, `secret-non-json-body super-secret`, "super-secret")
	defer srv.Close()
	rt := newProbeRuntime(t, srv, &fakeDB{}, newFakeCounter(nil))

	_, err := rt.ProbeCredits(context.Background(), srv.Client(), stateTestProvider(), probeToken())
	if err == nil {
		t.Fatal("非 2xx 应返回错误")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "secret-non-json-body") {
		t.Fatalf("非 JSON 正文不得回显: %q", err.Error())
	}
}

func TestProbeCreditsUnrecognisedShapeListsKeysOnly(t *testing.T) {
	srv := probeServer(t, http.StatusOK,
		`{"code":0,"msg":"ok","data":{"foo":"leaked-value","bar":1}}`, "super-secret")
	defer srv.Close()
	rt := newProbeRuntime(t, srv, &fakeDB{}, newFakeCounter(nil))

	_, err := rt.ProbeCredits(context.Background(), srv.Client(), stateTestProvider(), probeToken())
	if err == nil {
		t.Fatal("无法识别积分字段应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "积分字段未识别") {
		t.Fatalf("错误前缀不对: %q", msg)
	}
	for _, key := range []string{"foo", "bar", "code", "msg", "data"} {
		if !strings.Contains(msg, key) {
			t.Fatalf("错误应列出键名 %q: %q", key, msg)
		}
	}
	if strings.Contains(msg, "leaked-value") {
		t.Fatalf("错误串回显了字段值: %q", msg)
	}
}

func TestProbeCreditsPersistError(t *testing.T) {
	srv := probeServer(t, http.StatusOK, `{"data":{"remaining":1}}`, "super-secret")
	defer srv.Close()
	db := &fakeDB{creditErr: errors.New("db down")}
	rt := newProbeRuntime(t, srv, db, newFakeCounter(nil))

	credits, err := rt.ProbeCredits(context.Background(), srv.Client(), stateTestProvider(), probeToken())
	if err == nil {
		t.Fatal("持久化失败应返回错误")
	}
	if credits != 1 {
		t.Fatalf("应回传已解析积分, got %v", credits)
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Fatalf("错误应包装底层原因: %q", err.Error())
	}
}

func TestProbeCreditsRequiresContext(t *testing.T) {
	rt := newTestRuntime(&fakeDB{}, newFakeCounter(nil), stateTestNow)
	nonWB := &model.Provider{ID: 1, UserID: 2, OAuthProvider: "kimi"}
	if _, err := rt.ProbeCredits(context.Background(), nil, nonWB, probeToken()); !errors.Is(err, ErrNotWorkBuddy) {
		t.Fatalf("非 workbuddy 应返回 ErrNotWorkBuddy, got %v", err)
	}
	if _, err := rt.ProbeCredits(context.Background(), nil, nil, probeToken()); err == nil {
		t.Fatal("nil provider 应返回错误")
	}
	if _, err := rt.ProbeCredits(context.Background(), nil, stateTestProvider(), nil); err == nil {
		t.Fatal("nil token 应返回错误")
	}
}

// TestObserveSuccessClearsParkedReason 成功请求是停用状态的唯一恢复点：
// 转发侧 Admit 只读，因此「重新开启 / 额度重置 / 补足积分」后必须由
// observeSuccess 把 parked_reason 清空，否则面板会一直显示旧的停用原因。
func TestObserveSuccessClearsParkedReason(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{state: &model.WorkbuddyChannelState{
		UserID: p.UserID, ProviderID: p.ID,
		ParkedReason: reasonDisabled,
	}}
	rt := newTestRuntime(db, nil, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Success: true})

	if db.state.ParkedReason != "" {
		t.Fatalf("成功请求应清空 parked_reason, got %q", db.state.ParkedReason)
	}
	if db.state.LastOKAt == nil {
		t.Fatal("成功应同时写 last_ok_at")
	}
}

// TestObserveSuccessKeepsParkedReasonWhenAlreadyEmpty 反向：本来就为空时不做多余写入语义。
func TestObserveSuccessKeepsParkedReasonWhenAlreadyEmpty(t *testing.T) {
	p := stateTestProvider()
	db := &fakeDB{state: &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID}}
	rt := newTestRuntime(db, nil, stateTestNow)
	rt.Observe(Event{Provider: p, UpModel: "m", Success: true})
	if db.state.ParkedReason != "" {
		t.Fatalf("不应写入原因: %q", db.state.ParkedReason)
	}
}
