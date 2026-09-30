package workbuddy

// tasks_cn_test.go P4 国内版任务（growth / night / cat）单测。
//
// 全部走 httptest + 内存假 DB/假锁，不触碰 Postgres、Redis 与真实网络：
//   - 复用 state_test.go 的 fakeDB 与 jobs_test.go 的 fakeJobsDB（通过组合补充
//     UpdateWorkbuddyMeta，形成 cnMetaStore）；
//   - 端点接缝 cnTaskBaseURL / cnClaimWebURL 指向 httptest；
//   - sleep 接缝复用 jobs_test.go 的 stubSleep，测试零等待。
//
// 断言重点：请求序列、接取分批（20/20/5）、批内 (eventCode,id) 互异、跨重启去重、
// 桌面专属跳过、领奖 400 降级 web 域、夜间窗口、cat 分支顺序、INTL 零请求、
// 每步最多一次重试、正文不泄漏。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// ---- 假 DB（补充 Meta 持久化能力） ----

// fakeCNStore 组合 fakeJobsDB 并补上 UpdateWorkbuddyMeta（cnMetaStore）。
type fakeCNStore struct {
	*fakeJobsDB
}

func newFakeCNStore() *fakeCNStore { return &fakeCNStore{fakeJobsDB: newFakeJobsDB()} }

// UpdateWorkbuddyMeta 把去重池写回运行态 Meta（同一指针，便于跨 run 复用）。
func (f *fakeCNStore) UpdateWorkbuddyMeta(userID, providerID int64, key string, value any, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.statesByProvider[providerID]
	if st == nil {
		st = &model.WorkbuddyChannelState{UserID: userID, ProviderID: providerID, Meta: map[string]any{}}
		f.statesByProvider[providerID] = st
	}
	if st.Meta == nil {
		st.Meta = map[string]any{}
	}
	st.Meta[key] = value
	return nil
}

// 编译期断言：假 DB 同时满足框架接口与去重池接口。
var (
	_ stateStore  = (*fakeCNStore)(nil)
	_ jobsStore   = (*fakeCNStore)(nil)
	_ cnMetaStore = (*fakeCNStore)(nil)
)

// ---- 通用接缝 ----

// stubCNBases 把 chat 侧与 web 侧基址指向 httptest。
func stubCNBases(t *testing.T, base, web string) {
	t.Helper()
	prevBase, prevWeb := cnTaskBaseURL, cnClaimWebURL
	cnTaskBaseURL = func(Realm) string { return base }
	cnClaimWebURL = func() string { return web }
	t.Cleanup(func() {
		cnTaskBaseURL = prevBase
		cnClaimWebURL = prevWeb
	})
}

func newCNRuntime(store *fakeCNStore, now time.Time) *Runtime {
	r := NewRuntime(nil, nil)
	r.dbs = store
	r.Now = func() time.Time { return now }
	return r
}

func cnJobContext(r *Runtime, p *model.Provider, state *model.WorkbuddyChannelState, now time.Time) *JobContext {
	return &JobContext{
		Client:   &http.Client{},
		Now:      now,
		Provider: p,
		Setting: &model.WorkbuddySetting{
			UserID: p.UserID, Enabled: true,
			JobsEnabled: map[string]bool{"growth": true, "night": true, "cat": true},
		},
		Tok:     &oauth.TokenSet{AccessToken: "test-token", Extra: map[string]string{"uid": "u-test"}},
		State:   state,
		Runtime: r,
	}
}

// ---- 请求记录器 ----

type cnReq struct {
	Method string
	Path   string
	Host   string
	Body   string
	Header http.Header
}

type cnRecorder struct {
	mu   sync.Mutex
	reqs []cnReq
}

func (r *cnRecorder) add(q cnReq) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, q)
}

func (r *cnRecorder) snapshot() []cnReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cnReq(nil), r.reqs...)
}

func (r *cnRecorder) count() int { return len(r.snapshot()) }

func newCNServer(t *testing.T, respond func(cnReq) (int, string)) (*httptest.Server, *cnRecorder) {
	t.Helper()
	rec := &cnRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := cnReq{
			Method: r.Method,
			Path:   r.URL.Path,
			Host:   r.Host,
			Body:   string(body),
			Header: r.Header.Clone(),
		}
		rec.add(q)
		status, respBody := respond(q)
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

// reportIDs 抽出所有 /v2/report 批里带 id 的事件的 (eventCode,id)。
func reportIDs(t *testing.T, reqs []cnReq) [][2]string {
	t.Helper()
	var out [][2]string
	for _, q := range reqs {
		if q.Path != reportPath {
			continue
		}
		var events []map[string]any
		if err := json.Unmarshal([]byte(q.Body), &events); err != nil {
			t.Fatalf("report body 不是事件数组: %v", err)
		}
		for _, ev := range events {
			id, _ := ev["id"].(string)
			code, _ := ev["eventCode"].(string)
			out = append(out, [2]string{code, id})
		}
	}
	return out
}

func methodPath(q cnReq) string { return q.Method + " " + q.Path }

// ---- growth 快乐路径 ----

func TestGrowthHappyPathSequence(t *testing.T) {
	sleepRec := stubSleep(t)
	store := newFakeCNStore()
	state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
	store.statesByProvider[101] = state

	var getCount atomic.Int64
	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		switch {
		case q.Method == http.MethodGet && q.Path == growthTasksPath:
			if getCount.Add(1) == 1 {
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"expert_5","title":"使用专家助手","accept_status":"not_accepted","progress":{"current":0,"target":5}},
					{"task_code":"skill_1","title":"体验技能","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
			}
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"expert_5","accept_status":"completed","progress":{"current":5,"target":5}},
				{"task_code":"skill_1","accept_status":"completed","progress":{"current":1,"target":1}}]}}`
		case q.Method == http.MethodPost && q.Path == growthAcceptPath:
			return 200, `{"code":0,"data":{"results":[
				{"task_code":"expert_5","status":"accepted"},
				{"task_code":"skill_1","status":"accepted"}]}}`
		case q.Method == http.MethodPost && q.Path == reportPath:
			return 200, `{"code":0}`
		case q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim"):
			return 200, `{"code":0,"data":{"credit":3}}`
		}
		t.Errorf("未预期的请求: %s %s", q.Method, q.Path)
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	jc := cnJobContext(r, &p, state, testJobsNow)

	status, msg := runGrowthJob(context.Background(), jc)
	if status != jobStatusOK {
		t.Fatalf("status=%q msg=%q", status, msg)
	}

	reqs := rec.snapshot()
	got := make([]string, 0, len(reqs))
	for _, q := range reqs {
		got = append(got, methodPath(q))
	}
	want := []string{
		"GET " + growthTasksPath,
		"POST " + growthAcceptPath,
		"POST " + reportPath,
		"GET " + growthTasksPath,
		"POST /activity/growth/tasks/expert_5/claim",
		"POST " + reportPath,
		"GET " + growthTasksPath,
		"POST /activity/growth/tasks/skill_1/claim",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("请求序列不符:\n got=%v\nwant=%v", got, want)
	}

	// 接取批次（≤20）与顺序。
	var acc struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.Unmarshal([]byte(reqs[1].Body), &acc); err != nil {
		t.Fatalf("accept body: %v", err)
	}
	if len(acc.TaskCodes) != 2 || acc.TaskCodes[0] != "expert_5" || acc.TaskCodes[1] != "skill_1" {
		t.Fatalf("accept 任务码错误: %v", acc.TaskCodes)
	}

	// 批内 (eventCode,id) 互异（expert_5 批 5 个 id）。
	var firstReport []map[string]any
	if err := json.Unmarshal([]byte(reqs[2].Body), &firstReport); err != nil {
		t.Fatalf("report body: %v", err)
	}
	if len(firstReport) != 5 {
		t.Fatalf("expert_5 应上报 5 个事件, got %d", len(firstReport))
	}
	seen := map[string]bool{}
	for _, ev := range firstReport {
		if ev["eventCode"] != "expert_actual_use" {
			t.Fatalf("eventCode 错误: %v", ev["eventCode"])
		}
		id, _ := ev["id"].(string)
		if id == "" {
			t.Fatal("expert 事件必须有 id")
		}
		if seen[id] {
			t.Fatalf("同一批内 id 重复: %q", id)
		}
		seen[id] = true
	}

	// skill_1 批 1 个事件。
	var secondReport []map[string]any
	if err := json.Unmarshal([]byte(reqs[5].Body), &secondReport); err != nil {
		t.Fatalf("report body: %v", err)
	}
	if len(secondReport) != 1 || secondReport[0]["eventCode"] != "skill_info" {
		t.Fatalf("skill_1 上报错误: %+v", secondReport)
	}

	if sleepRec.count() == 0 {
		t.Fatal("report/claim 步骤之间应有节奏睡眠")
	}
	if !strings.Contains(msg, "上报=2") || !strings.Contains(msg, "领奖=2") {
		t.Fatalf("摘要计数错误: %q", msg)
	}
}

// ---- 接取分批 20/20/5 ----

func TestGrowthAcceptBatching(t *testing.T) {
	stubSleep(t)
	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		if q.Method == http.MethodPost && q.Path == growthAcceptPath {
			return 200, `{"code":0,"data":{"results":[]}}`
		}
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	store := newFakeCNStore()
	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	g, err := newGrowthRunner(context.Background(), cnJobContext(r, &p, nil, testJobsNow), RealmCN)
	if err != nil {
		t.Fatalf("newGrowthRunner: %v", err)
	}

	codes := make([]string, 45)
	for i := range codes {
		codes[i] = fmt.Sprintf("task_%d", i)
	}
	g.acceptTasks(codes)

	reqs := rec.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("45 个任务应分 3 批, got %d", len(reqs))
	}
	wantSizes := []int{20, 20, 5}
	for i, q := range reqs {
		var body struct {
			TaskCodes []string `json:"task_codes"`
		}
		if err := json.Unmarshal([]byte(q.Body), &body); err != nil {
			t.Fatalf("第 %d 批 body: %v", i, err)
		}
		if len(body.TaskCodes) != wantSizes[i] {
			t.Fatalf("第 %d 批大小 = %d, want %d", i, len(body.TaskCodes), wantSizes[i])
		}
	}
}

// ---- 事件 id 跨重启去重 ----

func TestGrowthEventIDDedupAcrossRuns(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
	store.statesByProvider[101] = state

	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		switch {
		case q.Method == http.MethodGet && q.Path == growthTasksPath:
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"expert_5","accept_status":"accepted","progress":{"current":0,"target":1}}]}}`
		case q.Method == http.MethodPost && q.Path == reportPath:
			return 200, `{"code":0}`
		case q.Method == http.MethodPost && q.Path == growthAcceptPath:
			return 200, `{"code":0,"data":{"results":[]}}`
		}
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	for run := 0; run < 2; run++ {
		jc := cnJobContext(r, &p, state, testJobsNow)
		if status, msg := runGrowthJob(context.Background(), jc); status != jobStatusOK {
			t.Fatalf("第 %d 轮 status=%q msg=%q", run, status, msg)
		}
	}

	ids := reportIDs(t, rec.snapshot())
	if len(ids) != 2 {
		t.Fatalf("两轮应各上报 1 个带 id 事件, got %d: %v", len(ids), ids)
	}
	if ids[0][1] == "" || ids[1][1] == "" {
		t.Fatalf("expert 事件 id 缺失: %v", ids)
	}
	if ids[0][1] == ids[1][1] {
		t.Fatalf("第二轮复用了已用 id: %v", ids)
	}

	raw, ok := state.Meta[metaKeyGrowthUsedEventIDs]
	if !ok {
		t.Fatal("去重池未写入 Meta")
	}
	if list, ok := raw.([]string); !ok || len(list) < 2 {
		t.Fatalf("去重池值异常: %#v", raw)
	}
}

// ---- 桌面专属任务跳过 ----

func TestGrowthSkipsDesktopOnlyTasks(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
	store.statesByProvider[101] = state

	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		switch {
		case q.Method == http.MethodGet && q.Path == growthTasksPath:
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"RichMeow_Chat","accept_status":"not_accepted","progress":{"current":0,"target":1}},
				{"task_code":"Library_read","accept_status":"not_accepted","progress":{"current":0,"target":1}},
				{"task_code":"Buddy_App","accept_status":"not_accepted","progress":{"current":0,"target":1}},
				{"task_code":"Buddy_App_QQ","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
		case q.Method == http.MethodPost && q.Path == growthAcceptPath:
			return 200, `{"code":0,"data":{"results":[
				{"task_code":"RichMeow_Chat","status":"accepted"},
				{"task_code":"Library_read","status":"accepted"},
				{"task_code":"Buddy_App","status":"accepted"},
				{"task_code":"Buddy_App_QQ","status":"accepted"}]}}`
		case q.Method == http.MethodPost && q.Path == reportPath:
			return 200, `{"code":0}`
		}
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, state, testJobsNow))
	if status != jobStatusOK {
		t.Fatalf("status=%q msg=%q", status, msg)
	}
	if !strings.Contains(msg, "跳过=4") {
		t.Fatalf("应跳过 4 个桌面专属任务: %q", msg)
	}
	for _, q := range rec.snapshot() {
		if q.Path == reportPath {
			t.Fatalf("桌面专属任务不应上报事件: %s", q.Body)
		}
	}
}

// ---- 领奖 400 降级 web 域 ----

func TestGrowthClaimFallbackOn400(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
	store.statesByProvider[101] = state

	srv1, rec1 := newCNServer(t, func(q cnReq) (int, string) {
		switch {
		case q.Method == http.MethodGet && q.Path == growthTasksPath:
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"expert_5","accept_status":"completed","progress":{"current":5,"target":5}}]}}`
		case q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim"):
			return 400, `{"code":400,"msg":"task not completed","echo":"super-secret"}`
		}
		return 404, `{}`
	})
	srv2, rec2 := newCNServer(t, func(q cnReq) (int, string) {
		if q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim") {
			return 200, `{"code":0,"data":{"credit":7}}`
		}
		return 404, `{}`
	})
	stubCNBases(t, srv1.URL, srv2.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, state, testJobsNow))
	if status != jobStatusOK {
		t.Fatalf("降级领奖应成功, status=%q msg=%q", status, msg)
	}
	if rec1.count() != 2 {
		t.Fatalf("chat 域应收到 清单+400领奖 共 2 次, got %d", rec1.count())
	}
	if rec2.count() != 1 {
		t.Fatalf("web 域应收到 1 次降级领奖, got %d", rec2.count())
	}
	web := rec2.snapshot()[0]
	if web.Method != http.MethodPost || web.Path != "/activity/growth/tasks/expert_5/claim" {
		t.Fatalf("web 降级请求形态错误: %s %s", web.Method, web.Path)
	}
	if web.Header.Get("X-Domain") != cnWebBase {
		t.Fatalf("web 降级应带 X-Domain=%s, got %q", cnWebBase, web.Header.Get("X-Domain"))
	}
	if web.Header.Get("X-User-Id") != "u-test" {
		t.Fatalf("web 降级应带 X-User-Id, got %q", web.Header.Get("X-User-Id"))
	}
	assertNoLeak(t, msg)
}

// ---- 夜间窗口 ----

func TestNightWindow(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		name     string
		hour     int
		min      int
		wantSkip bool
	}{
		{"22:59 窗口外", 22, 59, true},
		{"08:01 窗口外", 8, 1, true},
		{"23:30 窗口内", 23, 30, false},
		{"07:00 窗口内", 7, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubSleep(t)
			store := newFakeCNStore()
			state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
			store.statesByProvider[101] = state

			srv, rec := newCNServer(t, func(q cnReq) (int, string) {
				switch {
				case q.Method == http.MethodGet && q.Path == growthTasksPath:
					return 200, `{"code":0,"data":{"tasks":[
						{"task_code":"black_cat","accept_status":"accepted","progress":{"current":0,"target":3}}]}}`
				case q.Method == http.MethodPost && q.Path == reportPath:
					return 200, `{"code":0}`
				case q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim"):
					return 200, `{"code":0,"data":{"credit":1}}`
				}
				return 200, `{"code":0}`
			})
			stubCNBases(t, srv.URL, srv.URL)

			now := time.Date(2025, 3, 14, tc.hour, tc.min, 0, 0, cst)
			r := newCNRuntime(store, now)
			p := cnProvider(101, 10)
			status, msg := runNightJob(context.Background(), cnJobContext(r, &p, state, now))

			if tc.wantSkip {
				if status != jobStatusSkipped {
					t.Fatalf("窗口外应 skipped, got %q %q", status, msg)
				}
				if !strings.Contains(msg, "夜间窗口") {
					t.Fatalf("窗口外文案错误: %q", msg)
				}
				if rec.count() != 0 {
					t.Fatalf("窗口外不应发起请求, got %d", rec.count())
				}
				return
			}
			if status != jobStatusOK {
				t.Fatalf("窗口内应 ok, got %q %q", status, msg)
			}
			if rec.count() == 0 {
				t.Fatal("窗口内应发起请求")
			}
			hasReport := false
			for _, q := range rec.snapshot() {
				if q.Path == reportPath {
					hasReport = true
				}
			}
			if !hasReport {
				t.Fatal("窗口内应上报夜猫子事件")
			}
		})
	}
}

// ---- 猫猫旅行 ----

func TestCatTravel(t *testing.T) {
	t.Run("idle 今日已达上限 → ok", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			if q.Method == http.MethodGet && q.Path == travelStatusPath {
				return 200, `{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)
		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		status, msg := runCatJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
		if status != jobStatusOK || !strings.Contains(msg, "今日已完成旅行") {
			t.Fatalf("status=%q msg=%q", status, msg)
		}
		if rec.count() != 1 {
			t.Fatalf("只应查询一次状态, got %d", rec.count())
		}
	})

	t.Run("idle → config → depart 顺序", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == travelStatusPath:
				return 200, `{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`
			case q.Method == http.MethodGet && q.Path == travelConfigPath:
				return 200, `{"code":0,"data":{"locations":[{"id":7,"name":"巴黎"}]}}`
			case q.Method == http.MethodPost && q.Path == travelDepartPath:
				return 200, `{"code":0,"data":{"location":{"name":"巴黎"}}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)
		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		status, msg := runCatJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
		if status != jobStatusOK || !strings.Contains(msg, "出发") {
			t.Fatalf("status=%q msg=%q", status, msg)
		}
		reqs := rec.snapshot()
		got := []string{methodPath(reqs[0]), methodPath(reqs[1]), methodPath(reqs[2])}
		want := []string{
			"GET " + travelStatusPath,
			"GET " + travelConfigPath,
			"POST " + travelDepartPath,
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("请求序列不符: got=%v want=%v", got, want)
		}
		if strings.TrimSpace(reqs[2].Body) != `{"location_id":7}` {
			t.Fatalf("depart body = %q", reqs[2].Body)
		}
	})

	t.Run("arrived → claim", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == travelStatusPath:
				return 200, `{"code":0,"data":{"state":"arrived"}}`
			case q.Method == http.MethodPost && q.Path == travelClaimPath:
				return 200, `{"code":0,"data":{"reward_credit":9}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)
		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		status, msg := runCatJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
		if status != jobStatusOK || !strings.Contains(msg, "领奖成功") {
			t.Fatalf("status=%q msg=%q", status, msg)
		}
		reqs := rec.snapshot()
		if len(reqs) != 2 || reqs[0].Path != travelStatusPath || reqs[1].Path != travelClaimPath {
			t.Fatalf("arrived 分支顺序错误: %+v", reqs)
		}
	})
}

// ---- INTL 零请求 ----

func TestCNJobsIntlSkipped(t *testing.T) {
	stubSleep(t)
	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		t.Errorf("INTL 不应发起请求: %s %s", q.Method, q.Path)
		return 200, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(newFakeCNStore(), testJobsNow)
	p := model.Provider{ID: 101, UserID: 10, Enabled: true, OAuthProvider: ProviderIntl}
	jc := cnJobContext(r, &p, nil, testJobsNow)

	for _, fn := range []func(context.Context, *JobContext) (string, string){runGrowthJob, runNightJob, runCatJob} {
		status, msg := fn(context.Background(), jc)
		if status != jobStatusSkipped {
			t.Fatalf("INTL 应 skipped, got %q %q", status, msg)
		}
		if !strings.Contains(msg, "国际版无该任务") {
			t.Fatalf("INTL 文案错误: %q", msg)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("INTL 不应发起任何请求, got %d", rec.count())
	}
}

// ---- 每步最多一次重试 ----

func TestGrowthStepRetryCap(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		return 500, `{"error":{"message":"boom","code":"E500"},"echo":"super-secret"}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
	if status != jobStatusFailed {
		t.Fatalf("清单失败应 failed, got %q %q", status, msg)
	}
	if rec.count() != 2 {
		t.Fatalf("每步最多重试一次（共 2 次请求）, got %d", rec.count())
	}
	assertNoLeak(t, msg)
}

// ---- 领奖失败正文不泄漏 ----

func TestGrowthClaimRejectedNoBodyLeak(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
	store.statesByProvider[101] = state

	srv, _ := newCNServer(t, func(q cnReq) (int, string) {
		switch {
		case q.Method == http.MethodGet && q.Path == growthTasksPath:
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"expert_5","accept_status":"completed","progress":{"current":5,"target":5}}]}}`
		case q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim"):
			// 200 + 非 0 业务码，正文里塞一个疑似泄漏值。
			return 200, `{"code":400,"msg":"task not completed","echo":"super-secret"}`
		}
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, state, testJobsNow))
	if status != jobStatusFailed {
		t.Fatalf("领奖被拒应 failed, got %q %q", status, msg)
	}
	assertNoLeak(t, msg)
	if !strings.Contains(msg, "task not completed") {
		t.Fatalf("应保留结构化上游 msg: %q", msg)
	}
}
