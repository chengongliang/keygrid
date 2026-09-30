package workbuddy

// webagent_test.go 网页通道 ACP 打卡（webagent.go）的单测：全部走 httptest 假上游
// + 内存假 DB，不触碰 Postgres / Redis / 真实网络。
//
// 设计要点：
//   - 控制台 API 与沙箱 link 各起一个 httptest.Server；session 响应把 link 指回
//     沙箱 server（wb_accounts.py 的真实形态：link 是另一个域）；
//   - 基址通过未导出接缝 webBaseURL 指向 httptest；轮询间隔通过 webPollInterval 调短；
//   - SSE 读取协程的退出经 webSSEReaderExited 钩子观测（ctx 取消后必须退出）；
//   - 复用 jobs_test.go 的 fakeJobsDB / newJobsRuntime / enabledSetting / assertNoLeak，
//     只在本文件给 fakeJobsDB 补一个 UpdateWorkbuddyMeta 方法（webMetaStore 能力），
//     不新增第三个假 DB。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// ---- Meta 持久化捕获（复用 fakeJobsDB，不新增结构体字段） ----

var webMetaRec = struct {
	sync.Mutex
	vals map[string]any
}{}

// UpdateWorkbuddyMeta 给 fakeJobsDB 补上网页打卡需要的元数据写入能力，
// 记录到包级捕获器（fakeJobsDB 结构体定义在 jobs_test.go，不能加字段）。
func (f *fakeJobsDB) UpdateWorkbuddyMeta(_, _ int64, key string, value any, _ time.Time) error {
	webMetaRec.Lock()
	defer webMetaRec.Unlock()
	if webMetaRec.vals == nil {
		webMetaRec.vals = map[string]any{}
	}
	webMetaRec.vals[key] = value
	return nil
}

// 编译期断言：fakeJobsDB 满足 webMetaStore（webMetaSinkFor 的类型断言）。
var _ webMetaStore = (*fakeJobsDB)(nil)

func resetWebMeta() {
	webMetaRec.Lock()
	defer webMetaRec.Unlock()
	webMetaRec.vals = map[string]any{}
}

func webMetaSnapshot() map[string]any {
	webMetaRec.Lock()
	defer webMetaRec.Unlock()
	out := make(map[string]any, len(webMetaRec.vals))
	for k, v := range webMetaRec.vals {
		out[k] = v
	}
	return out
}

// ---- 请求记录 ----

type webCall struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

type webRecorder struct {
	mu    sync.Mutex
	calls []webCall
}

func (r *webRecorder) add(c webCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Header = c.Header.Clone()
	r.calls = append(r.calls, c)
}

func (r *webRecorder) snap() []webCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]webCall(nil), r.calls...)
}

func (r *webRecorder) count() int { return len(r.snap()) }

func firstCall(calls []webCall, pred func(webCall) bool) (webCall, bool) {
	for _, c := range calls {
		if pred(c) {
			return c, true
		}
	}
	return webCall{}, false
}

func callIndex(calls []webCall, pred func(webCall) bool) int {
	for i, c := range calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

// ---- 上游打桩 ----

type webStubOpts struct {
	createStatus  int
	createBody    string
	sessionStatus int
	sessionBody   string
	// statuses 依次作为轮询结果返回；用尽后重复最后一个。空 = 一直 completed。
	statuses       []string
	linkSSEStatus  int
	linkBody       string
	linkConnID     string
	linkPOSTStatus int
	// blockLinkGET link 的 SSE 响应写完头后一直挂住（用于 ctx 取消测试）。
	blockLinkGET bool
	onLinkPOST   func()
}

type webStub struct {
	opts         webStubOpts
	console      *httptest.Server
	link         *httptest.Server
	consoleCalls *webRecorder
	linkCalls    *webRecorder

	mu          sync.Mutex
	statusIdx   int
	statusCalls int
}

func newWebStub(t *testing.T, opts webStubOpts) *webStub {
	t.Helper()
	s := &webStub{opts: opts, consoleCalls: &webRecorder{}, linkCalls: &webRecorder{}}

	s.link = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.linkCalls.add(webCall{Method: r.Method, Path: r.URL.Path, Header: r.Header, Body: string(body)})

		if r.Method != http.MethodGet {
			if s.opts.onLinkPOST != nil {
				s.opts.onLinkPOST()
			}
			status := s.opts.linkPOSTStatus
			if status == 0 {
				status = http.StatusAccepted
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "{}")
			return
		}

		if s.opts.linkSSEStatus != 0 && s.opts.linkSSEStatus != http.StatusOK {
			w.WriteHeader(s.opts.linkSSEStatus)
			return
		}
		connID := s.opts.linkConnID
		if connID == "" {
			connID = "conn-1"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Acp-Connection-Id", connID)
		w.WriteHeader(http.StatusOK)
		if s.opts.linkBody != "" {
			_, _ = io.WriteString(w, s.opts.linkBody)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if s.opts.blockLinkGET {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(s.link.Close)

	s.console = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.consoleCalls.add(webCall{Method: r.Method, Path: r.URL.Path, Header: r.Header, Body: string(body)})
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/conversations/"):
			status := s.opts.createStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			text := s.opts.createBody
			if text == "" {
				text = `{"code":0,"msg":"OK","data":{"id":"conv-1"}}`
			}
			_, _ = io.WriteString(w, text)

		case strings.HasSuffix(r.URL.Path, "/session"):
			status := s.opts.sessionStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			text := s.opts.sessionBody
			if text == "" {
				text = `{"code":0,"msg":"OK","data":{"link":"` + s.link.URL + `/acp",` +
					`"token":"sandbox-token","sessionId":"sess-1","cwd":"/workspace"}}`
			}
			_, _ = io.WriteString(w, text)

		default: // GET /console/as/conversations/{id} 状态轮询
			s.mu.Lock()
			st := "completed"
			if len(s.opts.statuses) > 0 {
				if s.statusIdx >= len(s.opts.statuses) {
					st = s.opts.statuses[len(s.opts.statuses)-1]
				} else {
					st = s.opts.statuses[s.statusIdx]
				}
				s.statusIdx++
			}
			s.statusCalls++
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"status":"`+st+`"}}`)
		}
	}))
	t.Cleanup(s.console.Close)

	return s
}

func (s *webStub) pollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusCalls
}

// ---- 测试夹具 ----

func stubWebBase(t *testing.T, base string) {
	t.Helper()
	prev := webBaseURL
	webBaseURL = func(r Realm) (string, error) {
		if r != RealmIntl {
			t.Errorf("webBaseURL realm = %q, want intl", r)
		}
		return base, nil
	}
	t.Cleanup(func() { webBaseURL = prev })
}

func stubWebPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := webPollInterval
	webPollInterval = d
	t.Cleanup(func() { webPollInterval = prev })
}

// newWebJobContext 组装一个只依赖假 DB 的 JobContext（jc.Op 保持 nil，Meta 走假 DB）。
func newWebJobContext(t *testing.T, client *http.Client, provider model.Provider, accessToken string) (*JobContext, *fakeJobsDB) {
	t.Helper()
	db := newFakeJobsDB()
	rt := newJobsRuntime(db, nil)
	p := provider
	jc := &JobContext{
		Client:   client,
		Now:      testJobsNow,
		Provider: &p,
		Setting:  enabledSetting(p.UserID, "daily_chat_web"),
		Tok:      &oauth.TokenSet{AccessToken: accessToken, Extra: map[string]string{"uid": "u-1"}},
		Runtime:  rt,
		State:    &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID},
	}
	return jc, db
}

const webTestSSE = "event: message\n" +
	"data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"session_info_update\"}}}\n\n" +
	"data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"hi\"}}}}\n\n" +
	"data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n" +
	"data: [DONE]\n\n"

// ---- 1. happy path ----

func TestWebAgentHappyPath(t *testing.T) {
	resetWebMeta()
	stub := newWebStub(t, webStubOpts{
		// 先 pending 再 completed：断言「确实在轮询」而不是只看建会话成功。
		statuses: []string{"pending", "completed"},
		linkBody: webTestSSE,
	})
	stubWebBase(t, stub.console.URL)
	stubWebPollInterval(t, time.Millisecond)
	const token = "sandbox-secret-token"
	jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), token)

	status, msg := runDailyChatWebJob(context.Background(), jc)
	if status != jobStatusOK {
		t.Fatalf("status = %q (msg=%q), want ok", status, msg)
	}
	// 成功摘要只允许：状态字 + 段数 + 耗时。
	if !strings.Contains(msg, "completed") || !strings.Contains(msg, "segments=2") || !strings.Contains(msg, "elapsed=") {
		t.Fatalf("成功摘要形态错误: %q", msg)
	}
	assertNoLeak(t, msg)
	if stub.pollCount() < 2 {
		t.Fatalf("轮询次数 = %d, want >=2（pending → completed）", stub.pollCount())
	}

	// —— 控制台调用顺序：建会话 → 取沙箱 → 轮询 ——
	cc := stub.consoleCalls.snap()
	iCreate := callIndex(cc, func(c webCall) bool {
		return c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/conversations/")
	})
	iSession := callIndex(cc, func(c webCall) bool { return strings.HasSuffix(c.Path, "/session") })
	iPoll := callIndex(cc, func(c webCall) bool { return c.Method == http.MethodGet && !strings.HasSuffix(c.Path, "/session") })
	if iCreate < 0 || iSession < 0 || iPoll < 0 {
		t.Fatalf("控制台调用缺失: create=%d session=%d poll=%d (%+v)", iCreate, iSession, iPoll, cc)
	}
	if !(iCreate < iSession && iSession < iPoll) {
		t.Fatalf("控制台调用顺序错误: create=%d session=%d poll=%d", iCreate, iSession, iPoll)
	}

	// 建会话请求形态（对齐 wb_accounts.py:617-627）。
	create := cc[iCreate]
	if create.Path != WebConversationsPath {
		t.Fatalf("建会话路径 = %q, want %q", create.Path, WebConversationsPath)
	}
	if got := create.Header.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("建会话 Authorization = %q", got)
	}
	if got := create.Header.Get("X-User-Id"); got != "u-1" {
		t.Fatalf("建会话 X-User-Id = %q", got)
	}
	for k := range create.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-ide") {
			t.Fatalf("网页通道不应带 X-IDE-* 指纹头: %s", k)
		}
	}
	var createBody struct {
		Prompt             string `json:"prompt"`
		Model              string `json:"model"`
		ConversationOrigin string `json:"conversationOrigin"`
		Plugins            []struct {
			Name        string `json:"name"`
			Marketplace string `json:"marketplace"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(create.Body), &createBody); err != nil {
		t.Fatalf("建会话 body 不可解析: %v", err)
	}
	if createBody.Prompt == "" || createBody.Model != webDefaultModel || createBody.ConversationOrigin != webConversationOrigin {
		t.Fatalf("建会话 body 字段错误: %+v", createBody)
	}
	if len(createBody.Plugins) != 1 || createBody.Plugins[0].Name != webPluginName || createBody.Plugins[0].Marketplace != webPluginMarketplace {
		t.Fatalf("plugins 字段错误: %+v", createBody.Plugins)
	}

	// —— link 调用顺序：SSE GET → 三个 JSON-RPC POST ——
	lc := stub.linkCalls.snap()
	if len(lc) != 4 {
		t.Fatalf("link 调用数 = %d, want 1 GET + 3 POST (%+v)", len(lc), lc)
	}
	if lc[0].Method != http.MethodGet {
		t.Fatalf("第一次 link 调用 = %s, want GET", lc[0].Method)
	}
	if got := lc[0].Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("SSE GET Accept = %q", got)
	}
	if got := lc[0].Header.Get("Authorization"); got != "Bearer sandbox-token" {
		t.Fatalf("SSE GET Authorization = %q", got)
	}

	wantMethods := []string{"initialize", "session/load", "session/prompt"}
	for i, want := range wantMethods {
		call := lc[i+1]
		if call.Method != http.MethodPost {
			t.Fatalf("JSON-RPC 调用 %d = %s, want POST", i, call.Method)
		}
		if got := call.Header.Get("Acp-Connection-Id"); got != "conn-1" {
			t.Fatalf("JSON-RPC %s 的 Acp-Connection-Id = %q, want conn-1", want, got)
		}
		if got := call.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("JSON-RPC %s Content-Type = %q", want, got)
		}
		var rpc struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(call.Body), &rpc); err != nil {
			t.Fatalf("JSON-RPC body 不可解析: %v", err)
		}
		if rpc.JSONRPC != "2.0" || rpc.Method != want || rpc.ID != i+1 {
			t.Fatalf("JSON-RPC[%d] = %+v, want method=%s id=%d", i, rpc, want, i+1)
		}
		switch want {
		case "initialize":
			var p struct {
				ProtocolVersion    int `json:"protocolVersion"`
				ClientCapabilities struct {
					FS struct {
						ReadTextFile  bool `json:"readTextFile"`
						WriteTextFile bool `json:"writeTextFile"`
					} `json:"fs"`
					Terminal bool `json:"terminal"`
				} `json:"clientCapabilities"`
			}
			if err := json.Unmarshal(rpc.Params, &p); err != nil {
				t.Fatalf("initialize params 不可解析: %v", err)
			}
			if p.ProtocolVersion != webProtocolVersion {
				t.Fatalf("protocolVersion = %d, want %d", p.ProtocolVersion, webProtocolVersion)
			}
			if p.ClientCapabilities.Terminal || p.ClientCapabilities.FS.ReadTextFile || p.ClientCapabilities.FS.WriteTextFile {
				t.Fatalf("客户端能力必须全关: %+v", p.ClientCapabilities)
			}
		case "session/load":
			var p struct {
				SessionID  string `json:"sessionId"`
				Cwd        string `json:"cwd"`
				McpServers []any  `json:"mcpServers"`
			}
			if err := json.Unmarshal(rpc.Params, &p); err != nil {
				t.Fatalf("session/load params 不可解析: %v", err)
			}
			if p.SessionID != "sess-1" || p.Cwd != "/workspace" || len(p.McpServers) != 0 {
				t.Fatalf("session/load params = %+v", p)
			}
		case "session/prompt":
			var p struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"prompt"`
			}
			if err := json.Unmarshal(rpc.Params, &p); err != nil {
				t.Fatalf("session/prompt params 不可解析: %v", err)
			}
			if p.SessionID != "sess-1" || len(p.Prompt) != 1 || p.Prompt[0].Type != "text" || p.Prompt[0].Text != webPromptText {
				t.Fatalf("session/prompt params = %+v", p)
			}
		}
	}

	// 成功后 Meta 记下最后一条会话 id。
	meta := webMetaSnapshot()
	if got := meta[webMetaKeyLastConversation]; got != "conv-1" {
		t.Fatalf("Meta[%s] = %v, want conv-1", webMetaKeyLastConversation, got)
	}
}

// ---- 2. v1.6.9 陷阱：建了会话但这一轮失败 → 必须 failed ----

func TestWebAgentFailedStatusIsNotOK(t *testing.T) {
	resetWebMeta()
	stub := newWebStub(t, webStubOpts{
		statuses: []string{"working", "failed"},
		linkBody: webTestSSE,
	})
	stubWebBase(t, stub.console.URL)
	stubWebPollInterval(t, time.Millisecond)
	jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), "tok")

	status, msg := runDailyChatWebJob(context.Background(), jc)
	if status != jobStatusFailed {
		t.Fatalf("status = %q (msg=%q), want failed —— 建会话成功不等于打卡成功", status, msg)
	}
	if !strings.Contains(msg, "conv-1") {
		t.Fatalf("失败摘要应带 conversation id: %q", msg)
	}
	assertNoLeak(t, msg)
	// 建会话、取沙箱、三次 JSON-RPC 都应发生；只是最后状态是 failed。
	if stub.linkCalls.count() != 4 {
		t.Fatalf("link 调用数 = %d, want 4", stub.linkCalls.count())
	}
	if len(webMetaSnapshot()) != 0 {
		t.Fatal("失败不应写 last conversation Meta")
	}
}

// ---- 3. 建了会话但永远不 completed → 超时 failed ----

func TestWebAgentQueuedNeverCompletes(t *testing.T) {
	resetWebMeta()
	t.Setenv(webTurnTimeoutEnv, "300ms")
	stub := newWebStub(t, webStubOpts{
		statuses: []string{"pending"},
		linkBody: webTestSSE,
	})
	stubWebBase(t, stub.console.URL)
	stubWebPollInterval(t, 5*time.Millisecond)
	jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), "tok")

	start := time.Now()
	status, msg := runDailyChatWebJob(context.Background(), jc)
	elapsed := time.Since(start)
	if status != jobStatusFailed {
		t.Fatalf("status = %q (msg=%q), want failed", status, msg)
	}
	if !strings.Contains(msg, "conv-1") || !strings.Contains(msg, "没有跑完") {
		t.Fatalf("超时摘要错误: %q", msg)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("超时路径耗时 %v，应接近 300ms 而非 120s", elapsed)
	}
	assertNoLeak(t, msg)
}

// ---- 4. ctx 取消：及时返回且 SSE 读取协程退出 ----

func TestWebAgentContextCancelStopsAndReaderExits(t *testing.T) {
	resetWebMeta()
	// 用一个挂了 link 的 stub，取消时读取协程必须活着 → 被关闭后退出。
	posted := make(chan struct{})
	var postedOnce sync.Once
	stub := newWebStub(t, webStubOpts{
		statuses:     []string{"working"},
		linkBody:     webTestSSE,
		blockLinkGET: true,
		onLinkPOST:   func() { postedOnce.Do(func() { close(posted) }) },
	})
	stubWebBase(t, stub.console.URL)
	stubWebPollInterval(t, 5*time.Millisecond)

	exitSeen := make(chan struct{})
	var exitOnce sync.Once
	prevHook := webSSEReaderExited
	webSSEReaderExited = func() { exitOnce.Do(func() { close(exitSeen) }) }
	t.Cleanup(func() { webSSEReaderExited = prevHook })

	jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), "tok")
	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct {
		status string
		msg    string
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		st, m := runDailyChatWebJob(ctx, jc)
		done <- outcome{st, m}
	}()

	select {
	case <-posted:
	case <-time.After(3 * time.Second):
		t.Fatal("等不到 session/prompt POST，无法在流程中途取消")
	}
	cancel()

	select {
	case out := <-done:
		if out.status != jobStatusFailed {
			t.Fatalf("取消后 status = %q (msg=%q), want failed", out.status, out.msg)
		}
		if !strings.Contains(out.msg, "取消") {
			t.Fatalf("取消摘要应说明取消: %q", out.msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后函数没有及时返回")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消路径耗时 %v，过慢", elapsed)
	}

	// SSE 读取协程必须已退出（close() 等待 readerDone 后返回）。
	select {
	case <-exitSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE 读取协程在 ctx 取消后没有退出（泄漏）")
	}
}

// ---- 5. CN：零请求 skipped ----

func TestWebAgentCNRealmSkipped(t *testing.T) {
	stub := newWebStub(t, webStubOpts{})
	stubWebBase(t, stub.console.URL)
	jc, _ := newWebJobContext(t, stub.console.Client(), cnProvider(101, 10), "tok")

	status, msg := runDailyChatWebJob(context.Background(), jc)
	if status != jobStatusSkipped {
		t.Fatalf("CN 应 skipped, got %q (%q)", status, msg)
	}
	if !strings.Contains(msg, "国际版网页打卡") {
		t.Fatalf("skip 文案错误: %q", msg)
	}
	if stub.consoleCalls.count() != 0 || stub.linkCalls.count() != 0 {
		t.Fatalf("CN 不得发起任何请求: console=%d link=%d", stub.consoleCalls.count(), stub.linkCalls.count())
	}
}

// ---- 6. 传输错误（各步骤 500）→ failed，不 panic ----

func TestWebAgentTransportErrors(t *testing.T) {
	cases := []struct {
		name string
		opts webStubOpts
		want string
	}{
		{
			name: "建会话 500",
			opts: webStubOpts{
				createStatus: http.StatusInternalServerError,
				createBody:   `{"error":{"message":"boom","code":"E1"}}`,
			},
			want: "E1",
		},
		{
			name: "取沙箱 500",
			opts: webStubOpts{
				sessionStatus: http.StatusInternalServerError,
				sessionBody:   `{"error":{"message":"sess boom","code":"E2"}}`,
			},
			want: "conv-1",
		},
		{
			name: "SSE 连接 500",
			opts: webStubOpts{linkSSEStatus: http.StatusInternalServerError},
			want: "conv-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetWebMeta()
			stub := newWebStub(t, tc.opts)
			stubWebBase(t, stub.console.URL)
			stubWebPollInterval(t, time.Millisecond)
			jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), "tok")

			status, msg := runDailyChatWebJob(context.Background(), jc)
			if status != jobStatusFailed {
				t.Fatalf("status = %q (msg=%q), want failed", status, msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("失败摘要应含 %q: %q", tc.want, msg)
			}
			assertNoLeak(t, msg)
		})
	}
}

// ---- 7. 无秘密外泄（消息 + 持久化 Meta） ----

func TestWebAgentNoSecretLeak(t *testing.T) {
	t.Run("失败摘要不回显正文与 token", func(t *testing.T) {
		resetWebMeta()
		stub := newWebStub(t, webStubOpts{
			createStatus: http.StatusInternalServerError,
			createBody:   `{"error":{"message":"upstream boom","code":"E500"},"echo":"super-secret"}`,
		})
		stubWebBase(t, stub.console.URL)
		jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), "super-secret")

		status, msg := runDailyChatWebJob(context.Background(), jc)
		if status != jobStatusFailed {
			t.Fatalf("status = %q, want failed", status)
		}
		assertNoLeak(t, msg)
		if strings.Contains(msg, "upstream boom") == false {
			t.Fatalf("应保留结构化上游摘要: %q", msg)
		}
		if len(webMetaSnapshot()) != 0 {
			t.Fatal("失败不应写 Meta")
		}
	})

	t.Run("SSE 正文与 token 不进消息与 Meta", func(t *testing.T) {
		resetWebMeta()
		sse := "data: " + `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"super-secret"}}}}` + "\n\n" +
			"data: [DONE]\n\n"
		stub := newWebStub(t, webStubOpts{statuses: []string{"completed"}, linkBody: sse})
		stubWebBase(t, stub.console.URL)
		stubWebPollInterval(t, time.Millisecond)
		jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), "super-secret")

		status, msg := runDailyChatWebJob(context.Background(), jc)
		if status != jobStatusOK {
			t.Fatalf("status = %q (msg=%q), want ok", status, msg)
		}
		assertNoLeak(t, msg)
		for k, v := range webMetaSnapshot() {
			if s, _ := v.(string); strings.Contains(s, "super-secret") || strings.Contains(k, "super-secret") {
				t.Fatalf("Meta 泄漏秘密: %s=%v", k, v)
			}
		}
	})
}

// ---- 8. 沙箱未就绪（link/token 缺失）----

func TestWebAgentMissingSandbox(t *testing.T) {
	resetWebMeta()
	stub := newWebStub(t, webStubOpts{sessionBody: `{"code":0,"msg":"OK","data":{}}`})
	stubWebBase(t, stub.console.URL)
	jc, _ := newWebJobContext(t, stub.console.Client(), intlProvider(101, 10, "https://www.workbuddy.ai"), "tok")

	status, msg := runDailyChatWebJob(context.Background(), jc)
	if status != jobStatusFailed {
		t.Fatalf("status = %q (msg=%q), want failed", status, msg)
	}
	if !strings.Contains(msg, "沙箱") || !strings.Contains(msg, "conv-1") {
		t.Fatalf("沙箱缺失摘要错误: %q", msg)
	}
	if stub.linkCalls.count() != 0 {
		t.Fatal("沙箱未就绪不得连接 link")
	}
}
