package workbuddy

// jobs_test.go P3 任务框架的单测：全部走内存假 DB / 假锁 / httptest，不触碰
// Postgres、Redis 与真实网络。设计要点：
//
//   - 复用 state_test.go 的 fakeDB（stateStore）与 fakeCounter（counterStore），
//     fakeJobsDB 只补任务框架需要的 jobsStore 方法；
//   - 假锁实现 lockStore + counterStore，通过 Runtime.cs 注入（Runtime.locks 类型断言）；
//   - 端点接缝：checkinURL / dailyChatURL 指向 httptest，cell 指向固定 TokenSet；
//   - sleep 接缝记录抖动调用，测试断言「调度会抖动、手动不抖动」。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// testJobsNow 固定时间基准（UTC 正午，avoid 时区跨天边界）。
var testJobsNow = time.Date(2025, 3, 14, 12, 0, 0, 0, time.UTC)

// ---- 假 DB ----

type fakeJobsDB struct {
	*fakeDB // 复用 stateStore 的其余方法

	mu               sync.Mutex
	settingsByUser   map[int64]*model.WorkbuddySetting
	providersByUser  map[int64][]model.Provider
	statesByProvider map[int64]*model.WorkbuddyChannelState
	creds            map[int64]*model.Credential
	platform         map[string]string
	runs             []*model.WorkbuddyJobRun
	audits           []*model.AuditLog
	listErr          error
	settingErr       error
}

func newFakeJobsDB() *fakeJobsDB {
	return &fakeJobsDB{
		fakeDB:           &fakeDB{},
		settingsByUser:   map[int64]*model.WorkbuddySetting{},
		providersByUser:  map[int64][]model.Provider{},
		statesByProvider: map[int64]*model.WorkbuddyChannelState{},
		creds:            map[int64]*model.Credential{},
		platform:         map[string]string{},
	}
}

func (f *fakeJobsDB) GetWorkbuddySetting(userID int64) (*model.WorkbuddySetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.settingsByUser[userID]; ok {
		return s, nil
	}
	return &model.WorkbuddySetting{UserID: userID}, nil
}

func (f *fakeJobsDB) GetWorkbuddyChannelState(_, providerID int64) (*model.WorkbuddyChannelState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statesByProvider[providerID], nil
}

func (f *fakeJobsDB) ListWorkbuddySettingsWithJobs() ([]model.WorkbuddySetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]model.WorkbuddySetting, 0, len(f.settingsByUser))
	for _, s := range f.settingsByUser {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeJobsDB) ListProviders(userID int64) ([]model.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Provider(nil), f.providersByUser[userID]...), nil
}

func (f *fakeJobsDB) GetCredentialByProviderID(providerID int64) (*model.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creds[providerID], nil
}

func (f *fakeJobsDB) InsertWorkbuddyJobRun(r *model.WorkbuddyJobRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.runs = append(f.runs, &cp)
	return nil
}

func (f *fakeJobsDB) GetSetting(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settingErr != nil {
		return "", f.settingErr
	}
	return f.platform[key], nil
}

func (f *fakeJobsDB) CreateAuditLog(l *model.AuditLog) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *l
	f.audits = append(f.audits, &cp)
}

func (f *fakeJobsDB) runSnapshot() []*model.WorkbuddyJobRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*model.WorkbuddyJobRun(nil), f.runs...)
}

// 编译期断言：假 DB 同时满足 stateStore（注入 Runtime.dbs）与 jobsStore。
var (
	_ stateStore = (*fakeJobsDB)(nil)
	_ jobsStore  = (*fakeJobsDB)(nil)
)

// ---- 假锁 ----

type fakeLockStore struct {
	*fakeCounter // 同时提供 counterStore（Runtime.cs 的类型）

	mu       sync.Mutex
	held     map[string]bool
	acquired []string
	denied   bool
	err      error
}

func newFakeLockStore() *fakeLockStore {
	return &fakeLockStore{fakeCounter: newFakeCounter(nil), held: map[string]bool{}}
}

func (f *fakeLockStore) Acquire(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, key)
	if f.err != nil {
		return false, f.err
	}
	if f.denied || f.held[key] {
		return false, nil
	}
	f.held[key] = true
	return true, nil
}

func (f *fakeLockStore) Release(_ context.Context, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, key)
}

func (f *fakeLockStore) acquiredKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acquired...)
}

// 编译期断言：假锁同时满足 lockStore 与 counterStore。
var (
	_ lockStore    = (*fakeLockStore)(nil)
	_ counterStore = (*fakeLockStore)(nil)
)

// ---- 通用接缝 ----

func newJobsRuntime(db *fakeJobsDB, locks *fakeLockStore) *Runtime {
	r := NewRuntime(nil, nil)
	r.dbs = db
	if locks != nil {
		r.cs = locks
	}
	r.Now = func() time.Time { return testJobsNow }
	return r
}

func stubJobToken(t *testing.T) {
	t.Helper()
	prev := jobTokenDecoder
	jobTokenDecoder = func(*model.Credential) (*oauth.TokenSet, error) {
		return &oauth.TokenSet{AccessToken: "test-token", Extra: map[string]string{"uid": "u-test"}}, nil
	}
	t.Cleanup(func() { jobTokenDecoder = prev })
}

type sleepRecorder struct {
	mu    sync.Mutex
	calls []time.Duration
}

func (s *sleepRecorder) fn(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, d)
}

func (s *sleepRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func stubSleep(t *testing.T) *sleepRecorder {
	t.Helper()
	prev := sleep
	rec := &sleepRecorder{}
	sleep = rec.fn
	t.Cleanup(func() { sleep = prev })
	return rec
}

func stubCheckinURL(t *testing.T, base string) {
	t.Helper()
	prev := checkinURL
	checkinURL = func(Realm) string { return base + BillingPathCheckin }
	t.Cleanup(func() { checkinURL = prev })
}

func stubJobTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := jobTimeoutOverride
	jobTimeoutOverride = d
	t.Cleanup(func() { jobTimeoutOverride = prev })
}

// capturedReq 上游看到的请求（只取形状与鉴权，测试不断言正文内容）。
type capturedReq struct {
	Method      string
	Path        string
	ContentType string
	Auth        string
	IDEType     string
	Body        string
}

type reqRecorder struct {
	mu   sync.Mutex
	reqs []capturedReq
}

func (r *reqRecorder) add(c capturedReq) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, c)
}

func (r *reqRecorder) snapshot() []capturedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedReq(nil), r.reqs...)
}

func (r *reqRecorder) count() int { return len(r.snapshot()) }

func newUpstreamServer(t *testing.T, respond func(capturedReq) (int, string)) (*httptest.Server, *reqRecorder) {
	t.Helper()
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := capturedReq{
			Method:      r.Method,
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			Auth:        r.Header.Get("Authorization"),
			IDEType:     r.Header.Get("X-IDE-Type"),
			Body:        string(body),
		}
		rec.add(c)
		status, respBody := respond(c)
		if status == 0 {
			status = http.StatusOK
		}
		if respBody == "" {
			respBody = "{}"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// ---- 构造辅助 ----

func enabledSetting(userID int64, jobs ...string) *model.WorkbuddySetting {
	m := map[string]bool{}
	for _, j := range jobs {
		m[j] = true
	}
	return &model.WorkbuddySetting{UserID: userID, Enabled: true, JobsEnabled: m}
}

func cnProvider(id, userID int64) model.Provider {
	return model.Provider{ID: id, UserID: userID, Enabled: true, OAuthProvider: ProviderCN, Kind: "oauth"}
}

func intlProvider(id, userID int64, baseURL string) model.Provider {
	return model.Provider{ID: id, UserID: userID, Enabled: true, OAuthProvider: ProviderIntl, Kind: "oauth", BaseURL: baseURL}
}

const dailyChatSSE = "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// ---- 测试 ----

func TestJobKeysOrderAndCopy(t *testing.T) {
	got := JobKeys()
	want := []string{"checkin", "credits", "daily_chat"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JobKeys() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if again := JobKeys(); again[0] != "checkin" {
		t.Fatalf("JobKeys() 返回了内部切片别名: %v", again)
	}
}

func TestJobsAllowedPrecedence(t *testing.T) {
	t.Run("平台设置开启优先", func(t *testing.T) {
		db := newFakeJobsDB()
		db.platform[model.SettingWorkbuddyJobs] = "1"
		if !newJobsRuntime(db, nil).JobsAllowed() {
			t.Fatal("平台设置=1 应允许")
		}
	})
	t.Run("平台设置关闭优先于 env", func(t *testing.T) {
		db := newFakeJobsDB()
		db.platform[model.SettingWorkbuddyJobs] = "0"
		if newJobsRuntime(db, nil).JobsAllowed() {
			t.Fatal("平台设置=0 应禁止")
		}
	})
	t.Run("未设置跟随 env（默认关）", func(t *testing.T) {
		db := newFakeJobsDB()
		r := newJobsRuntime(db, nil)
		if got, want := r.JobsAllowed(), JobsEnabled(); got != want {
			t.Fatalf("未设置时应跟随 env: got %v want %v", got, want)
		}
	})
	t.Run("读取错误保守关闭", func(t *testing.T) {
		db := newFakeJobsDB()
		db.settingErr = io.ErrUnexpectedEOF
		if newJobsRuntime(db, nil).JobsAllowed() {
			t.Fatal("读取错误应按关闭处理")
		}
	})
	t.Run("无 store 只跟 env", func(t *testing.T) {
		r := NewRuntime(nil, nil)
		if r.JobsAllowed() != JobsEnabled() {
			t.Fatal("无 op/db 时应只跟随 env")
		}
	})
}

func TestRunJobsUnknownKey(t *testing.T) {
	_, err := newJobsRuntime(newFakeJobsDB(), nil).RunJobs(context.Background(), 1, "nope", 0, "manual")
	if err == nil || !strings.Contains(err.Error(), "unknown job key") {
		t.Fatalf("未知 jobKey 应报错, got %v", err)
	}
}

func TestKillSwitchTickAndManual(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "0"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	srv, rec := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, `{"code":0}` })
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	stubSleep(t)
	r := newJobsRuntime(db, nil)

	n, err := r.Tick(context.Background(), testJobsNow)
	if err != nil || n != 0 {
		t.Fatalf("kill switch 关闭时 Tick 应返回 (0,nil), got (%d,%v)", n, err)
	}
	if len(db.runSnapshot()) != 0 {
		t.Fatal("kill switch 关闭时不应写执行记录")
	}
	if rec.count() != 0 {
		t.Fatal("kill switch 关闭时不应发上游请求")
	}

	_, err = r.RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err == nil || !strings.Contains(err.Error(), "已关闭") {
		t.Fatalf("手动入口应返回明确的关闭错误, got %v", err)
	}
}

func TestCheckinCNHappyPath(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	srv, rec := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, `{"code":0,"data":{"checkedIn":false}}` })
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	stubSleep(t)

	results, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusOK {
		t.Fatalf("期望 ok, got %+v", results)
	}
	reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("期望 1 次上游请求, got %d", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodPost || got.Path != BillingPathCheckin {
		t.Fatalf("请求形态错误: %+v", got)
	}
	if got.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q", got.ContentType)
	}
	if got.Auth != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got.Auth)
	}
	if got.Body != "{}" {
		t.Fatalf("签到 body 应为 {}, got %q", got.Body)
	}
	runs := db.runSnapshot()
	if len(runs) != 1 || runs[0].Status != jobStatusOK || runs[0].JobKey != "checkin" {
		t.Fatalf("执行记录错误: %+v", runs)
	}
	if runs[0].ProviderID == nil || *runs[0].ProviderID != 101 || runs[0].UserID != 10 {
		t.Fatalf("执行记录归属错误: %+v", runs[0])
	}
	if len(db.audits) != 1 || db.audits[0].Event != "workbuddy.job.checkin" {
		t.Fatalf("审计记录错误: %+v", db.audits)
	}
}

func TestCheckinIntlSkippedNoRequest(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{intlProvider(101, 10, "https://www.workbuddy.ai")}
	srv, rec := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, `{"code":0}` })
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	stubSleep(t)

	results, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusSkipped {
		t.Fatalf("国际版签到应 skipped, got %+v", results)
	}
	if !strings.Contains(results[0].Message, "国际版无签到端点") {
		t.Fatalf("skip 文案错误: %q", results[0].Message)
	}
	if rec.count() != 0 {
		t.Fatalf("国际版不应发起任何请求, got %d", rec.count())
	}
}

func TestIdempotencyLockSameDay(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	srv, rec := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, `{"code":0}` })
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	stubSleep(t)
	locks := newFakeLockStore()
	r := newJobsRuntime(db, locks)

	first, err := r.RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err != nil || len(first) != 1 || first[0].Status != jobStatusOK {
		t.Fatalf("首次执行应 ok: %+v err=%v", first, err)
	}
	second, err := r.RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err != nil || len(second) != 1 || second[0].Status != jobStatusSkipped {
		t.Fatalf("同日二次执行应 skipped: %+v err=%v", second, err)
	}
	if second[0].Message != jobMsgAlreadyRan {
		t.Fatalf("幂等文案错误: %q", second[0].Message)
	}
	if rec.count() != 1 {
		t.Fatalf("上游只应被调用一次, got %d", rec.count())
	}
	if runs := db.runSnapshot(); len(runs) != 1 {
		t.Fatalf("只应写一条执行记录, got %d", len(runs))
	}
	keys := locks.acquiredKeys()
	if len(keys) != 2 || !strings.Contains(keys[0], ":job:checkin:10:101:") {
		t.Fatalf("幂等键形状错误: %v", keys)
	}
}

func TestLockDeniedYieldsSkipped(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	srv, rec := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, `{"code":0}` })
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	stubSleep(t)
	locks := newFakeLockStore()
	locks.denied = true

	results, err := newJobsRuntime(db, locks).RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusSkipped || results[0].Message != jobMsgAlreadyRan {
		t.Fatalf("锁被占用应 skipped: %+v", results)
	}
	if rec.count() != 0 {
		t.Fatal("锁被占用时不应发请求")
	}
	if len(db.runSnapshot()) != 0 {
		t.Fatal("锁被占用时不应写执行记录")
	}
}

func TestTickPerUserIsolation(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.settingsByUser[20] = enabledSetting(20, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	db.providersByUser[20] = []model.Provider{cnProvider(202, 20)}
	srv, rec := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, `{"code":0}` })
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	rec2 := stubSleep(t)

	n, err := newJobsRuntime(db, nil).Tick(context.Background(), testJobsNow)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 2 {
		t.Fatalf("Tick 应执行 2 次, got %d", n)
	}
	if rec.count() != 2 {
		t.Fatalf("上游应收到 2 次请求, got %d", rec.count())
	}
	if rec2.count() == 0 {
		t.Fatal("同一 tick 内同任务多账号应触发错峰抖动")
	}

	owner := map[int64]int64{101: 10, 202: 20}
	runs := db.runSnapshot()
	if len(runs) != 2 {
		t.Fatalf("应写 2 条执行记录, got %d", len(runs))
	}
	seen := map[int64]bool{}
	for _, run := range runs {
		if run.ProviderID == nil {
			t.Fatalf("执行记录缺 provider: %+v", run)
		}
		if owner[*run.ProviderID] != run.UserID {
			t.Fatalf("跨用户串扰: run user=%d provider=%d", run.UserID, *run.ProviderID)
		}
		seen[*run.ProviderID] = true
	}
	if !seen[101] || !seen[202] {
		t.Fatalf("未覆盖两个用户各自的渠道: %v", seen)
	}
}

func TestManualRunJobsDoesNotSleep(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10), cnProvider(102, 10)}
	srv, _ := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, `{"code":0}` })
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	rec := stubSleep(t)

	if _, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "checkin", 0, "manual"); err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("手动路径不应错峰睡眠, got %d", rec.count())
	}
}

func TestDailyChatIntlSuccess(t *testing.T) {
	t.Setenv(dailyChatModelEnv, "")
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "daily_chat")
	srv, rec := newUpstreamServer(t, func(capturedReq) (int, string) { return http.StatusOK, dailyChatSSE })
	db.providersByUser[10] = []model.Provider{intlProvider(101, 10, srv.URL)}
	stubJobToken(t)
	stubSleep(t)

	results, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "daily_chat", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusOK {
		t.Fatalf("期望 ok, got %+v", results)
	}
	reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("期望 1 次请求, got %d", len(reqs))
	}
	got := reqs[0]
	if got.Path != ChatPath {
		t.Fatalf("请求路径应为 %s, got %q", ChatPath, got.Path)
	}
	if got.IDEType == "" {
		t.Fatal("缺少 X-IDE-Type 身分头")
	}
	if got.Auth != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got.Auth)
	}
	if !strings.Contains(got.Body, `"stream":true`) {
		t.Fatalf("请求体应带 stream:true, got %s", got.Body)
	}
	if !strings.Contains(got.Body, `"model":"`) {
		t.Fatalf("请求体应带 model, got %s", got.Body)
	}
	runs := db.runSnapshot()
	if len(runs) != 1 || runs[0].Status != jobStatusOK {
		t.Fatalf("应写一条 ok 执行记录: %+v", runs)
	}
	if !strings.Contains(runs[0].Message, "segments=") {
		t.Fatalf("摘要应含段数: %q", runs[0].Message)
	}
}

func TestDailyChatNon2xxFailedNoBodyLeak(t *testing.T) {
	t.Setenv(dailyChatModelEnv, "")
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "daily_chat")
	srv, _ := newUpstreamServer(t, func(capturedReq) (int, string) {
		return http.StatusInternalServerError, `{"error":{"message":"upstream boom","code":"E500"},"echo":"super-secret"}`
	})
	db.providersByUser[10] = []model.Provider{intlProvider(101, 10, srv.URL)}
	stubJobToken(t)
	stubSleep(t)

	results, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "daily_chat", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusFailed {
		t.Fatalf("非 2xx 应 failed, got %+v", results)
	}
	assertNoLeak(t, results[0].Message)
	assertNoLeak(t, db.runSnapshot()[0].Message)
	if !strings.Contains(results[0].Message, "E500") {
		t.Fatalf("应保留结构化上游错误码: %q", results[0].Message)
	}
}

func TestCheckinNon2xxFailedNoBodyLeak(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	srv, _ := newUpstreamServer(t, func(capturedReq) (int, string) {
		return http.StatusBadGateway, `{"error":{"message":"bad gateway"},"raw":"super-secret"}`
	})
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	stubSleep(t)

	results, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusFailed {
		t.Fatalf("非 2xx 应 failed, got %+v", results)
	}
	assertNoLeak(t, results[0].Message)
	assertNoLeak(t, db.runSnapshot()[0].Message)
}

func TestJobTimeout(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "checkin")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	srv, _ := newUpstreamServer(t, func(capturedReq) (int, string) {
		time.Sleep(300 * time.Millisecond)
		return http.StatusOK, `{"code":0}`
	})
	stubCheckinURL(t, srv.URL)
	stubJobToken(t)
	stubSleep(t)
	stubJobTimeout(t, 50*time.Millisecond)

	results, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "checkin", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusFailed {
		t.Fatalf("超时应 failed, got %+v", results)
	}
	if !strings.Contains(results[0].Message, "超时") {
		t.Fatalf("超时文案错误: %q", results[0].Message)
	}
}

func TestCreditsNotWired(t *testing.T) {
	db := newFakeJobsDB()
	db.platform[model.SettingWorkbuddyJobs] = "1"
	db.settingsByUser[10] = enabledSetting(10, "credits")
	db.providersByUser[10] = []model.Provider{cnProvider(101, 10)}
	stubJobToken(t)
	stubSleep(t)

	results, err := newJobsRuntime(db, nil).RunJobs(context.Background(), 10, "credits", 0, "manual")
	if err != nil {
		t.Fatalf("RunJobs: %v", err)
	}
	if len(results) != 1 || results[0].Status != jobStatusFailed {
		t.Fatalf("Op 未接线应 failed, got %+v", results)
	}
	assertNoLeak(t, results[0].Message)
}

// assertNoLeak 校验摘要契约：≤512 rune、不含上游正文令牌 super-secret。
func assertNoLeak(t *testing.T, msg string) {
	t.Helper()
	if len([]rune(msg)) > maxJobMessageLen {
		t.Fatalf("Message 超长（%d rune）: %q", len([]rune(msg)), msg)
	}
	if strings.Contains(msg, "super-secret") {
		t.Fatalf("Message 泄漏了上游正文: %q", msg)
	}
}
