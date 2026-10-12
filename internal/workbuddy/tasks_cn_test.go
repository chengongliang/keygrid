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
	"encoding/base64"
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
			switch getCount.Add(1) {
			case 1:
				// 初始：两个任务都未接取。
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"Expert_team_use_3","title":"使用专家团队","accept_status":"not_accepted","progress":{"current":0,"target":3}},
					{"task_code":"skill_1","title":"体验技能","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
			case 2:
				// 接取后复核：都已接取，进度仍为 0。
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"Expert_team_use_3","accept_status":"accepted","progress":{"current":0,"target":3}},
					{"task_code":"skill_1","accept_status":"accepted","progress":{"current":0,"target":1}}]}}`
			case 3:
				// team 轮询：上报后进度达标。
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"Expert_team_use_3","accept_status":"completed","progress":{"current":3,"target":3}},
					{"task_code":"skill_1","accept_status":"accepted","progress":{"current":0,"target":1}}]}}`
			default:
				// skill 轮询：达标。
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"Expert_team_use_3","accept_status":"completed","progress":{"current":3,"target":3}},
					{"task_code":"skill_1","accept_status":"completed","progress":{"current":1,"target":1}}]}}`
			}
		case q.Method == http.MethodPost && q.Path == growthAcceptPath:
			return 200, `{"code":0,"data":{"results":[
				{"task_code":"Expert_team_use_3","status":"accepted"},
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
		"GET " + growthTasksPath,
		"POST " + reportPath,
		"GET " + growthTasksPath,
		"POST /activity/growth/tasks/Expert_team_use_3/claim",
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
	if len(acc.TaskCodes) != 2 || acc.TaskCodes[0] != "Expert_team_use_3" || acc.TaskCodes[1] != "skill_1" {
		t.Fatalf("accept 任务码错误: %v", acc.TaskCodes)
	}

	// team 任务 target=3，应一次上报 3 个互异 id 的 expert 事件。
	var firstReport []map[string]any
	if err := json.Unmarshal([]byte(reqs[3].Body), &firstReport); err != nil {
		t.Fatalf("report body: %v", err)
	}
	if len(firstReport) != 3 {
		t.Fatalf("Expert_team_use_3 应上报 3 个事件, got %d", len(firstReport))
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
	if err := json.Unmarshal([]byte(reqs[6].Body), &secondReport); err != nil {
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
	// 接取后复核会重列清单，因此应有 4 次 GET（初始 / 复核 / 2 次轮询）。
	if getCount.Load() != 4 {
		t.Fatalf("GET 次数 = %d, want 4", getCount.Load())
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

// ---- 新增：上游 v1.7 行为对齐 ----

// cnCountPath 统计某路径的请求数。
func cnCountPath(reqs []cnReq, path string) int {
	n := 0
	for _, q := range reqs {
		if q.Path == path {
			n++
		}
	}
	return n
}

// cnTestJWT 构造仅用于只读判定的未签名 JWT（payload 段 base64url）。
func cnTestJWT(payload string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestGrowthAutoProcessedBeforeAccept 钉住 wb_tasks.py:650-697 的顺序：
// auto 任务（first_buddy）先点亮领奖，再批量接取非 auto 未接任务。
func TestGrowthAutoProcessedBeforeAccept(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
	store.statesByProvider[101] = state

	var getCount atomic.Int64
	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		switch {
		case q.Method == http.MethodGet && q.Path == growthTasksPath:
			switch getCount.Add(1) {
			case 1:
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"first_buddy","task_type":"auto","accept_status":"not_accepted","progress":{"current":0,"target":1}},
					{"task_code":"expert_5","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
			case 2:
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"first_buddy","task_type":"auto","accept_status":"completed","progress":{"current":1,"target":1}},
					{"task_code":"expert_5","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
			case 3:
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"first_buddy","task_type":"auto","accept_status":"completed","progress":{"current":1,"target":1}},
					{"task_code":"expert_5","accept_status":"accepted","progress":{"current":0,"target":1}}]}}`
			default:
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"first_buddy","task_type":"auto","accept_status":"completed","progress":{"current":1,"target":1}},
					{"task_code":"expert_5","accept_status":"completed","progress":{"current":1,"target":1}}]}}`
			}
		case q.Method == http.MethodPost && q.Path == growthAcceptPath:
			return 200, `{"code":0,"data":{"results":[{"task_code":"expert_5","status":"accepted"}]}}`
		case q.Method == http.MethodPost && q.Path == reportPath:
			return 200, `{"code":0}`
		case q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim"):
			return 200, `{"code":0,"data":{"credit":1}}`
		}
		t.Errorf("未预期请求: %s %s", q.Method, q.Path)
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, state, testJobsNow))
	if status != jobStatusOK {
		t.Fatalf("status=%q msg=%q", status, msg)
	}

	reqs := rec.snapshot()
	firstReport, acceptAt := -1, -1
	for i, q := range reqs {
		if q.Path == reportPath && firstReport < 0 {
			firstReport = i
		}
		if q.Path == growthAcceptPath && acceptAt < 0 {
			acceptAt = i
		}
	}
	if firstReport < 0 || acceptAt < 0 {
		t.Fatalf("缺少 report/accept 请求: %+v", reqs)
	}
	if firstReport > acceptAt {
		t.Fatalf("auto 任务应先于接取点亮: firstReport=%d accept=%d", firstReport, acceptAt)
	}
	var acc struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.Unmarshal([]byte(reqs[acceptAt].Body), &acc); err != nil {
		t.Fatalf("accept body: %v", err)
	}
	if len(acc.TaskCodes) != 1 || acc.TaskCodes[0] != "expert_5" {
		t.Fatalf("accept 应只含 expert_5（auto 不接取）: %v", acc.TaskCodes)
	}
	var evs []map[string]any
	if err := json.Unmarshal([]byte(reqs[firstReport].Body), &evs); err != nil {
		t.Fatalf("report body: %v", err)
	}
	if len(evs) != 1 || evs[0]["eventCode"] != "chat_request_send" {
		t.Fatalf("first_buddy 事件错误: %+v", evs)
	}
	if !strings.Contains(msg, "领奖=2") {
		t.Fatalf("auto 与 single 都应领奖: %q", msg)
	}
}

// TestGrowthProgressPoll 钉住 wb_tasks.py:579-620：
// target=3 上报 3 次，进度轮询达标才领奖；6 次轮询仍未达标则顺延不领奖。
func TestGrowthProgressPoll(t *testing.T) {
	t.Run("target=3 上报3次且达标后领奖", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		var getCount atomic.Int64
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == growthTasksPath:
				switch getCount.Add(1) {
				case 1:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"expert_5","accept_status":"accepted","progress":{"current":0,"target":3}}]}}`
				case 2:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"expert_5","accept_status":"accepted","progress":{"current":1,"target":3}}]}}`
				case 3:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"expert_5","accept_status":"accepted","progress":{"current":2,"target":3}}]}}`
				default:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"expert_5","accept_status":"completed","progress":{"current":3,"target":3}}]}}`
				}
			case q.Method == http.MethodPost && q.Path == reportPath:
				return 200, `{"code":0}`
			case q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim"):
				return 200, `{"code":0,"data":{"credit":1}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)

		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
		if status != jobStatusOK {
			t.Fatalf("status=%q msg=%q", status, msg)
		}
		reqs := rec.snapshot()
		var evs []map[string]any
		for _, q := range reqs {
			if q.Path == reportPath {
				if err := json.Unmarshal([]byte(q.Body), &evs); err != nil {
					t.Fatalf("report body: %v", err)
				}
			}
		}
		if len(evs) != 3 {
			t.Fatalf("target=3 应上报 3 次事件, got %d", len(evs))
		}
		if got := cnCountPath(reqs, "/activity/growth/tasks/expert_5/claim"); got != 1 {
			t.Fatalf("达标后应领奖 1 次, got %d", got)
		}
		if !strings.Contains(msg, "上报=1") || !strings.Contains(msg, "领奖=1") {
			t.Fatalf("摘要计数错误: %q", msg)
		}
	})

	t.Run("6 次轮询仍未达标则顺延不领奖", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		var getCount atomic.Int64
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == growthTasksPath:
				n := int(getCount.Add(1))
				if n == 1 {
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"expert_5","accept_status":"accepted","progress":{"current":0,"target":10}}]}}`
				}
				return 200, fmt.Sprintf(`{"code":0,"data":{"tasks":[{"task_code":"expert_5","accept_status":"accepted","progress":{"current":%d,"target":10}}]}}`, n-1)
			case q.Method == http.MethodPost && q.Path == reportPath:
				return 200, `{"code":0}`
			case q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/claim"):
				t.Errorf("进度未达标不应领奖")
				return 200, `{"code":0}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)

		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
		if status != jobStatusOK {
			t.Fatalf("status=%q msg=%q", status, msg)
		}
		reqs := rec.snapshot()
		if got := cnCountPath(reqs, "/activity/growth/tasks/expert_5/claim"); got != 0 {
			t.Fatalf("进度未达标不应领奖, got %d", got)
		}
		// 初始 1 次 + 6 次轮询 = 7 次 GET。
		if getCount.Load() != 7 {
			t.Fatalf("应轮询 6 次（共 7 次 GET）, got %d", getCount.Load())
		}
		if !strings.Contains(msg, "上报=1") || !strings.Contains(msg, "领奖=0") {
			t.Fatalf("摘要计数错误: %q", msg)
		}
	})
}

// TestGrowthAcceptReasonsSurfaced 钉住 wb_tasks.py:214/235/248/261：
// 接取失败原因进入汇总 Message，且只保留结构化上游 msg（不泄漏正文）。
func TestGrowthAcceptReasonsSurfaced(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
	store.statesByProvider[101] = state

	srv, _ := newCNServer(t, func(q cnReq) (int, string) {
		switch {
		case q.Method == http.MethodGet && q.Path == growthTasksPath:
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"expert_5","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
		case q.Method == http.MethodPost && q.Path == growthAcceptPath:
			return 200, `{"code":400,"msg":"task not completed","echo":"super-secret"}`
		}
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, state, testJobsNow))
	if status != jobStatusOK {
		t.Fatalf("接取失败但任务被跳过，按参考实现整体 ok: status=%q msg=%q", status, msg)
	}
	if !strings.Contains(msg, "接取原因") {
		t.Fatalf("应汇总接取原因: %q", msg)
	}
	if !strings.Contains(msg, "task not completed") {
		t.Fatalf("应保留结构化上游 msg: %q", msg)
	}
	assertNoLeak(t, msg)
}

// TestFirstBuddySpecAndEnsureBuddy 钉住 wb_tasks.py:54-56/419-434：
// first_buddy 规格变更为 chat/300/领取一只 Buddy，ensure_buddy 完成并领取后为 true。
func TestFirstBuddySpecAndEnsureBuddy(t *testing.T) {
	spec, ok := growthTaskSpecs["first_buddy"]
	if !ok || spec.Kind != "chat" || spec.Reward != 300 || spec.Name != "领取一只 Buddy" {
		t.Fatalf("first_buddy 规格未对齐: %+v", spec)
	}

	t.Run("ensureBuddy 完成并领取后返回 true", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
		store.statesByProvider[101] = state
		var getCount atomic.Int64
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == growthTasksPath:
				switch getCount.Add(1) {
				case 1:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"first_buddy","task_type":"auto","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
				case 2:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"first_buddy","task_type":"auto","accept_status":"completed","progress":{"current":1,"target":1}}]}}`
				default:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"first_buddy","task_type":"auto","accept_status":"claimed","progress":{"current":1,"target":1}}]}}`
				}
			case q.Method == http.MethodPost && q.Path == reportPath:
				return 200, `{"code":0}`
			case q.Method == http.MethodPost && q.Path == "/activity/growth/tasks/first_buddy/claim":
				return 200, `{"code":0,"data":{"credit":300}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)

		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		g, err := newGrowthRunner(context.Background(), cnJobContext(r, &p, state, testJobsNow), RealmCN)
		if err != nil {
			t.Fatalf("newGrowthRunner: %v", err)
		}
		if !g.ensureBuddy() {
			t.Fatal("ensureBuddy 应返回 true")
		}
		reqs := rec.snapshot()
		if cnCountPath(reqs, "/activity/growth/tasks/first_buddy/claim") != 1 {
			t.Fatalf("first_buddy 应领奖 1 次: %+v", reqs)
		}
		found := false
		for _, q := range reqs {
			if q.Path == reportPath && strings.Contains(q.Body, "chat_request_send") {
				found = true
			}
		}
		if !found {
			t.Fatal("ensureBuddy 应上报 chat 事件")
		}
	})

	t.Run("清单缺少 first_buddy 返回 false", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		srv, _ := newCNServer(t, func(q cnReq) (int, string) {
			if q.Method == http.MethodGet && q.Path == growthTasksPath {
				return 200, `{"code":0,"data":{"tasks":[]}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)
		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		g, err := newGrowthRunner(context.Background(), cnJobContext(r, &p, nil, testJobsNow), RealmCN)
		if err != nil {
			t.Fatalf("newGrowthRunner: %v", err)
		}
		if g.ensureBuddy() {
			t.Fatal("缺少 first_buddy 应返回 false")
		}
	})
}

// TestCatDepartRetryAfterNoActiveBuddy 钉住 wb_tasks.py:504-513：
// depart 报 "no active buddy" 时先补 first_buddy 再重试一次；补不上则给出清晰失败。
func TestCatDepartRetryAfterNoActiveBuddy(t *testing.T) {
	t.Run("补 first_buddy 后重试成功", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		state := &model.WorkbuddyChannelState{UserID: 10, ProviderID: 101, Meta: map[string]any{}}
		store.statesByProvider[101] = state
		var departs, taskGets atomic.Int64
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == travelStatusPath:
				return 200, `{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`
			case q.Method == http.MethodGet && q.Path == travelConfigPath:
				return 200, `{"code":0,"data":{"locations":[{"id":7,"name":"巴黎"}]}}`
			case q.Method == http.MethodPost && q.Path == travelDepartPath:
				if departs.Add(1) == 1 {
					return 200, `{"code":4001,"msg":"no active buddy"}`
				}
				return 200, `{"code":0,"data":{"location":{"name":"巴黎"}}}`
			case q.Method == http.MethodGet && q.Path == growthTasksPath:
				switch taskGets.Add(1) {
				case 1:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"first_buddy","task_type":"auto","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
				case 2:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"first_buddy","task_type":"auto","accept_status":"completed","progress":{"current":1,"target":1}}]}}`
				default:
					return 200, `{"code":0,"data":{"tasks":[{"task_code":"first_buddy","task_type":"auto","accept_status":"claimed","progress":{"current":1,"target":1}}]}}`
				}
			case q.Method == http.MethodPost && q.Path == reportPath:
				return 200, `{"code":0}`
			case q.Method == http.MethodPost && q.Path == "/activity/growth/tasks/first_buddy/claim":
				return 200, `{"code":0,"data":{"credit":300}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)

		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		status, msg := runCatJob(context.Background(), cnJobContext(r, &p, state, testJobsNow))
		if status != jobStatusOK || !strings.Contains(msg, "出发") {
			t.Fatalf("status=%q msg=%q", status, msg)
		}
		if departs.Load() != 2 {
			t.Fatalf("应在补 Buddy 后重试 depart, got %d 次", departs.Load())
		}
		if cnCountPath(rec.snapshot(), "/activity/growth/tasks/first_buddy/claim") != 1 {
			t.Fatal("应先完成并领取 first_buddy")
		}
	})

	t.Run("无 first_buddy 时给出清晰失败", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		var departs atomic.Int64
		srv, _ := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == travelStatusPath:
				return 200, `{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`
			case q.Method == http.MethodGet && q.Path == travelConfigPath:
				return 200, `{"code":0,"data":{"locations":[{"id":7,"name":"巴黎"}]}}`
			case q.Method == http.MethodPost && q.Path == travelDepartPath:
				departs.Add(1)
				return 200, `{"code":4001,"msg":"no active buddy"}`
			case q.Method == http.MethodGet && q.Path == growthTasksPath:
				return 200, `{"code":0,"data":{"tasks":[]}}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)

		r := newCNRuntime(store, testJobsNow)
		p := cnProvider(101, 10)
		status, msg := runCatJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
		if status != jobStatusFailed {
			t.Fatalf("补不上 Buddy 应 failed: status=%q msg=%q", status, msg)
		}
		if !strings.Contains(msg, "还没有 Buddy") {
			t.Fatalf("应给出清晰失败文案: %q", msg)
		}
		if departs.Load() != 1 {
			t.Fatalf("补不上 Buddy 不应重试 depart, got %d 次", departs.Load())
		}
	})
}

// TestGrowthSkipsWechatTask 钉住 wb_tasks.py:57-59：新增的公众号任务不可伪造，
// 必须跳过且绝不上报事件。
func TestGrowthSkipsWechatTask(t *testing.T) {
	stubSleep(t)
	store := newFakeCNStore()
	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		if q.Method == http.MethodGet && q.Path == growthTasksPath {
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"wb_wechat_oa_subscribe_task","accept_status":"not_accepted","progress":{"current":0,"target":1}}]}}`
		}
		return 404, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(store, testJobsNow)
	p := cnProvider(101, 10)
	status, msg := runGrowthJob(context.Background(), cnJobContext(r, &p, nil, testJobsNow))
	if status != jobStatusOK {
		t.Fatalf("status=%q msg=%q", status, msg)
	}
	if !strings.Contains(msg, "跳过=1") {
		t.Fatalf("公众号任务应记为跳过: %q", msg)
	}
	if rec.count() != 1 {
		t.Fatalf("公众号任务不应有接取/上报/领奖请求, got %d", rec.count())
	}
}

// TestCNEnterpriseSkipped 钉住 wb_tasks.py:638/717/465：企业版无成长体系，
// 三个 job 都 skipped 且零请求。
func TestCNEnterpriseSkipped(t *testing.T) {
	stubSleep(t)
	srv, rec := newCNServer(t, func(q cnReq) (int, string) {
		t.Errorf("企业版不应发起请求: %s %s", q.Method, q.Path)
		return 200, `{}`
	})
	stubCNBases(t, srv.URL, srv.URL)

	r := newCNRuntime(newFakeCNStore(), testJobsNow)
	p := cnProvider(101, 10)
	tok := &oauth.TokenSet{
		AccessToken: cnTestJWT(`{"enterprise_id":"ent-9"}`),
		Extra:       map[string]string{"uid": "u-test"},
	}
	for _, fn := range []func(context.Context, *JobContext) (string, string){runGrowthJob, runNightJob, runCatJob} {
		jc := cnJobContext(r, &p, nil, testJobsNow)
		jc.Tok = tok
		status, msg := fn(context.Background(), jc)
		if status != jobStatusSkipped || !strings.Contains(msg, "企业版") {
			t.Fatalf("企业版应 skipped: status=%q msg=%q", status, msg)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("企业版不应发起任何请求, got %d", rec.count())
	}
}

// TestNightAcceptsUnaccepted 钉住 wb_tasks.py:730-737：夜猫子任务未接取时先接取，
// 接取失败则今晚跳过并返回失败。
func TestNightAcceptsUnaccepted(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	now := time.Date(2025, 3, 14, 23, 30, 0, 0, cst)

	t.Run("未接取则先接取再上报", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == growthTasksPath:
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"black_cat","accept_status":"not_accepted","progress":{"current":0,"target":3}}]}}`
			case q.Method == http.MethodPost && q.Path == growthAcceptPath:
				return 200, `{"code":0,"data":{"results":[{"task_code":"black_cat","status":"accepted"}]}}`
			case q.Method == http.MethodPost && q.Path == reportPath:
				return 200, `{"code":0}`
			}
			return 200, `{"code":0}`
		})
		stubCNBases(t, srv.URL, srv.URL)

		r := newCNRuntime(store, now)
		p := cnProvider(101, 10)
		status, msg := runNightJob(context.Background(), cnJobContext(r, &p, nil, now))
		if status != jobStatusOK {
			t.Fatalf("应接取并上报成功: status=%q msg=%q", status, msg)
		}
		reqs := rec.snapshot()
		acceptAt, reportAt := -1, -1
		for i, q := range reqs {
			if q.Path == growthAcceptPath && acceptAt < 0 {
				acceptAt = i
			}
			if q.Path == reportPath && reportAt < 0 {
				reportAt = i
			}
		}
		if acceptAt < 0 || reportAt < 0 || acceptAt > reportAt {
			t.Fatalf("应先接取后上报: accept=%d report=%d", acceptAt, reportAt)
		}
		var acc struct {
			TaskCodes []string `json:"task_codes"`
		}
		if err := json.Unmarshal([]byte(reqs[acceptAt].Body), &acc); err != nil {
			t.Fatalf("accept body: %v", err)
		}
		if len(acc.TaskCodes) != 1 || acc.TaskCodes[0] != "black_cat" {
			t.Fatalf("accept 应只含 black_cat: %v", acc.TaskCodes)
		}
	})

	t.Run("接取失败则跳过且不报请求", func(t *testing.T) {
		stubSleep(t)
		store := newFakeCNStore()
		srv, rec := newCNServer(t, func(q cnReq) (int, string) {
			switch {
			case q.Method == http.MethodGet && q.Path == growthTasksPath:
				return 200, `{"code":0,"data":{"tasks":[
					{"task_code":"black_cat","accept_status":"not_accepted","progress":{"current":0,"target":3}}]}}`
			case q.Method == http.MethodPost && q.Path == growthAcceptPath:
				return 200, `{"code":400,"msg":"task not completed","echo":"super-secret"}`
			}
			return 404, `{}`
		})
		stubCNBases(t, srv.URL, srv.URL)

		r := newCNRuntime(store, now)
		p := cnProvider(101, 10)
		status, msg := runNightJob(context.Background(), cnJobContext(r, &p, nil, now))
		if status != jobStatusFailed || !strings.Contains(msg, "未接取") {
			t.Fatalf("接取失败应 failed: status=%q msg=%q", status, msg)
		}
		if cnCountPath(rec.snapshot(), reportPath) != 0 {
			t.Fatal("未接取不应上报夜猫子事件")
		}
		assertNoLeak(t, msg)
	})
}

// TestCNIsEnterpriseSources 覆盖企业版判定的两个来源（token claim / Extra）。
func TestCNIsEnterpriseSources(t *testing.T) {
	cases := []struct {
		name string
		tok  *oauth.TokenSet
		want bool
	}{
		{"nil", nil, false},
		{"非 JWT token", &oauth.TokenSet{AccessToken: "test-token"}, false},
		{"JWT claim 空", &oauth.TokenSet{AccessToken: cnTestJWT(`{"uid":"u"}`)}, false},
		{"JWT claim enterprise_id", &oauth.TokenSet{AccessToken: cnTestJWT(`{"enterprise_id":"e1"}`)}, true},
		{"Extra enterprise_id", &oauth.TokenSet{Extra: map[string]string{"enterprise_id": "e2"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cnIsEnterprise(tc.tok); got != tc.want {
				t.Fatalf("cnIsEnterprise() = %v, want %v", got, tc.want)
			}
		})
	}
}
