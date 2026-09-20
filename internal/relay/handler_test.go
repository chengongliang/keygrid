package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

func TestBuildCandidates(t *testing.T) {
	providers := []model.Provider{
		{ID: 1, Name: "low", Kind: "api_key", Enabled: true, Priority: 1, ModelMap: map[string]string{"gpt-4o": "gpt-4o-2024"}},
		{ID: 2, Name: "high", Kind: "api_key", Enabled: true, Priority: 10},
		{ID: 3, Name: "disabled", Kind: "api_key", Enabled: false, Priority: 100},
		{ID: 4, Name: "oauth", Kind: "oauth", Enabled: true, Priority: 100},
	}

	cands := buildCandidates(providers, "gpt-4o", nil)
	// oauth 渠道也参与路由（凭据 relay 侧解密/刷新）→ 3 个候选
	if len(cands) != 3 {
		t.Fatalf("expected 3 candidates (enabled api_key+oauth), got %d", len(cands))
	}
	// priority 降序：oauth(100) → high(10) → low(1)
	if cands[0].provider.ID != 4 || cands[1].provider.ID != 2 || cands[2].provider.ID != 1 {
		t.Fatalf("expected [4,2,1] by priority desc, got [%d,%d,%d]",
			cands[0].provider.ID, cands[1].provider.ID, cands[2].provider.ID)
	}
	if cands[1].upModel != "gpt-4o" {
		t.Fatalf("no map → passthrough, got %q", cands[1].upModel)
	}
	if cands[2].upModel != "gpt-4o-2024" {
		t.Fatalf("map match failed, got %q", cands[2].upModel)
	}

	// 只有带 model_map 的渠道支持该模型
	narrow := []model.Provider{
		{ID: 7, Kind: "api_key", Enabled: true, ModelMap: map[string]string{"gpt-4o": "upstream-alias"}},
	}
	cands = buildCandidates(narrow, "other-model", nil)
	if len(cands) != 0 {
		t.Fatalf("expected 0 candidates for unmapped model, got %d", len(cands))
	}

	// 全部不可用
	empty := []model.Provider{{ID: 8, Kind: "api_key", Enabled: false}}
	if cands = buildCandidates(empty, "x", nil); len(cands) != 0 {
		t.Fatal("expected no candidates when no channel enabled")
	}

	// key 级渠道白名单：只保留白名单内渠道，优先级顺序仍生效
	if cands = buildCandidates(providers, "gpt-4o", []int64{1}); len(cands) != 1 || cands[0].provider.ID != 1 {
		t.Fatalf("provider whitelist [1] must keep only channel 1, got %+v", cands)
	}
	cands = buildCandidates(providers, "gpt-4o", []int64{1, 2})
	if len(cands) != 2 || cands[0].provider.ID != 2 || cands[1].provider.ID != 1 {
		t.Fatalf("provider whitelist [1,2] must keep 2,1 by priority, got %+v", cands)
	}
	// 白名单指向禁用渠道 → 无候选（渠道池为空由 relayCore 转 403 提示）
	if cands = buildCandidates(providers, "gpt-4o", []int64{3}); len(cands) != 0 {
		t.Fatalf("disabled channel in whitelist must yield no candidates, got %+v", cands)
	}
	// 白名单不含任何支持该模型的渠道 → 无候选
	if cands = buildCandidates(narrow, "gpt-4o", []int64{99}); len(cands) != 0 {
		t.Fatalf("whitelist outside channel pool must yield no candidates, got %+v", cands)
	}
}

func TestBuildUpstreamBody(t *testing.T) {
	orig := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)

	// 无改名：原样透传
	if got := buildUpstreamBody(orig, "m", "m"); string(got) != string(orig) {
		t.Fatalf("expected passthrough, got %s", got)
	}
	// 改名：仅替换 model 字段
	got := buildUpstreamBody(orig, "gpt-4o", "upstream-alias")
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if m["model"] != "upstream-alias" || m["temperature"] != 0.5 {
		t.Fatalf("unexpected body: %s", got)
	}
	if len(m["messages"].([]any)) != 1 {
		t.Fatal("messages must survive rewrite")
	}

	// developer role（Responses/Codex 语义）→ system：无改名路径也必须归一化，
	// 否则 DeepSeek 等 OpenAI 兼容上游会 400（unknown variant `developer`）
	dev := []byte(`{"model":"m","messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`)
	got = buildUpstreamBody(dev, "m", "m")
	var dm map[string]any
	if err := json.Unmarshal(got, &dm); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	dmsgs, _ := dm["messages"].([]any)
	if dmsgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer must become system: %s", got)
	}
	if dmsgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("other roles must survive: %s", got)
	}
}

// ---- 熔断状态机 ----

// 渠道级开关：熔断检测默认关闭（nil / 零值都不参与），只有显式开启才生效。
func TestBreakerCheckEnabled(t *testing.T) {
	if breakerCheckEnabled(nil) {
		t.Fatal("nil provider must not enable breaker check")
	}
	if breakerCheckEnabled(&model.Provider{}) {
		t.Fatal("zero value must default to disabled（默认否）")
	}
	if !breakerCheckEnabled(&model.Provider{BreakerCheck: true}) {
		t.Fatal("explicitly enabled must be detected")
	}
}

// fakeClock 可推进的测试时钟（滑动窗口 / 探测租约都需要穿越时间）。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestState() (*State, *fakeClock) {
	c := &fakeClock{t: time.Unix(1700000000, 0)}
	st := NewState()
	st.now = c.now
	return st, c
}

func TestCircuitBreaker(t *testing.T) {
	st, clk := newTestState()
	for i := 0; i < defaultFailThreshold; i++ {
		st.OnFailure()
	}
	if state, _ := st.Snapshot(); state != "open" {
		t.Fatalf("expected open after %d failures, got %s", defaultFailThreshold, state)
	}
	if ok, _ := st.Allow(); ok {
		t.Fatal("open breaker must reject")
	}

	// 到期 → half-open，放行 1 个试探（带探测令牌）
	clk.advance(defaultOpenDuration + time.Second)
	ok, probe := st.Allow()
	if !ok || probe == 0 {
		t.Fatalf("half-open must allow one probe with token, got ok=%v token=%d", ok, probe)
	}
	if ok, _ := st.Allow(); ok {
		t.Fatal("half-open must allow only one probe")
	}
	st.OnSuccess()
	if state, _ := st.Snapshot(); state != "closed" {
		t.Fatalf("expected closed after success, got %s", state)
	}

	// half-open 试探失败 → 重新 open
	for i := 0; i < defaultFailThreshold; i++ {
		st.OnFailure()
	}
	if state, _ := st.Snapshot(); state != "open" {
		t.Fatalf("precondition: expected open, got %s", state)
	}
	clk.advance(defaultOpenDuration + time.Second)
	if ok, _ := st.Allow(); !ok {
		t.Fatal("half-open must allow probe")
	}
	st.OnFailure() // probe fails → re-trip
	if state, _ := st.Snapshot(); state != "open" {
		t.Fatalf("failed probe must re-trip, got %s", state)
	}
}

// 滑动窗口：滑出窗口的旧失败不再凑阈值（旧实现是累计计数，跨小时的零星失败也能熔断）。
func TestCircuitBreakerSlidingWindow(t *testing.T) {
	st, clk := newTestState()
	st.OnFailure()
	st.OnFailure()
	clk.advance(defaultFailWindow + time.Second) // 前两条滑出窗口
	st.OnFailure()
	st.OnFailure()
	if state, fails := st.Snapshot(); state != "closed" || fails != 2 {
		t.Fatalf("stale failures must not count, got state=%s fails=%d", state, fails)
	}
	// 窗口内凑满阈值 → 熔断
	for i := 0; i < defaultFailThreshold-2; i++ {
		st.OnFailure()
	}
	if state, _ := st.Snapshot(); state != "open" {
		t.Fatalf("expected open, got %s", state)
	}
}

// 探测租约：调用方漏归还（历史缺陷：客户端断开/凭据失效路径直接 return）
// 不能让渠道永久卡在 half-open。
func TestCircuitBreakerProbeLease(t *testing.T) {
	st, clk := newTestState()
	for i := 0; i < defaultFailThreshold; i++ {
		st.OnFailure()
	}
	clk.advance(defaultOpenDuration + time.Second)

	ok, probe := st.Allow()
	if !ok || probe == 0 {
		t.Fatal("half-open must allow probe")
	}
	if ok, _ := st.Allow(); ok {
		t.Fatal("in-flight probe must block other requests")
	}
	// 租约到期 → 探测蒸发，重新签发（渠道不会永久不可用）
	clk.advance(defaultProbeTimeout + time.Second)
	ok, probe2 := st.Allow()
	if !ok || probe2 == 0 || probe2 == probe {
		t.Fatalf("expired lease must re-issue a probe, got ok=%v token=%d/%d", ok, probe2, probe)
	}
	// 晚到的旧探测归还不能误伤在途的新探测
	st.ReleaseOne(probe)
	if ok, _ := st.Allow(); ok {
		t.Fatal("stale release must not free the live probe")
	}
	st.OnSuccess()
	if state, _ := st.Snapshot(); state != "closed" {
		t.Fatalf("expected closed, got %s", state)
	}
}

// 探测资格显式归还后，下一个请求可以立刻再试探（不必等租约到期）。
func TestCircuitBreakerProbeRelease(t *testing.T) {
	st, clk := newTestState()
	for i := 0; i < defaultFailThreshold; i++ {
		st.OnFailure()
	}
	clk.advance(defaultOpenDuration + time.Second)

	ok, probe := st.Allow()
	if !ok || probe == 0 {
		t.Fatal("half-open must allow probe")
	}
	st.ReleaseOne(probe)
	if ok, probe2 := st.Allow(); !ok || probe2 == probe || probe2 == 0 {
		t.Fatalf("released probe must allow a fresh one, got ok=%v token=%d", ok, probe2)
	}
}

func TestBreakerRegistry(t *testing.T) {
	b := NewBreaker()
	for i := 0; i < defaultFailThreshold; i++ {
		b.OnFailure(42)
	}
	if ok, _ := b.Allow(42); ok {
		t.Fatal("channel 42 should be open")
	}
	if ok, _ := b.Allow(43); !ok {
		t.Fatal("channel 43 should be independent")
	}
}

// Reset 手动恢复（admin 重置熔断）。
func TestBreakerReset(t *testing.T) {
	b := NewBreaker()
	for i := 0; i < defaultFailThreshold; i++ {
		b.OnFailure(7)
	}
	if ok, _ := b.Allow(7); ok {
		t.Fatal("channel 7 should be open")
	}
	b.Reset(7)
	if ok, _ := b.Allow(7); !ok {
		t.Fatal("reset must close the breaker")
	}
	if b.SnapshotByID(7) != "closed" {
		t.Fatalf("snapshot after reset = %s", b.SnapshotByID(7))
	}
}

func TestClientGoneDetection(t *testing.T) {
	// SSE 透传可返回哨兵，也可能直接透出 net/http 的 context canceled 包装错误。
	if !clientGone(context.Canceled) {
		t.Fatal("context cancellation must be detected")
	}
	if !clientGone(errors.New("read upstream: context canceled")) {
		t.Fatal("wrapped cancellation text must be detected")
	}
	if clientGone(errors.New("upstream timeout")) {
		t.Fatal("real upstream failure must not be treated as client cancellation")
	}
}

// ---- SSE 解析 ----

func TestParseSSELine(t *testing.T) {
	if _, ok := parseSSELine([]byte("data: [DONE]")); ok {
		t.Fatal("[DONE] must not parse as usage chunk")
	}
	if _, ok := parseSSELine([]byte(": comment")); ok {
		t.Fatal("comment lines must be skipped")
	}
	ev, ok := parseSSELine([]byte(`data: {"id":"x"}`))
	if !ok || string(ev) != `{"id":"x"}` {
		t.Fatalf("data line parse failed: %v %v", ev, ok)
	}
}

func TestExtractChunkUsage(t *testing.T) {
	// 无 usage 的 chunk
	if _, found := extractChunkUsage(json.RawMessage(`{"choices":[{"delta":{"content":"hi"}}]}`)); found {
		t.Fatal("chunk without usage must not report found")
	}
	// 有 usage 的 chunk
	u, found := extractChunkUsage(json.RawMessage(`{"model":"m","usage":{"prompt_tokens":3,"completion_tokens":5}}`))
	if !found || u.PromptTokens != 3 || u.CompletionTokens != 5 {
		t.Fatalf("usage extraction failed: %+v found=%v", u, found)
	}
}

// ---- failover 判定 ----

func TestShouldFailover(t *testing.T) {
	if !shouldFailover(0, context.Canceled) {
		t.Fatal("errors must failover")
	}
	if !shouldFailover(http.StatusTooManyRequests, nil) || !shouldFailover(502, nil) {
		t.Fatal("429/5xx must failover")
	}
	if shouldFailover(200, nil) || shouldFailover(400, nil) {
		t.Fatal("2xx/4xx(非429) must not failover")
	}
}

// 熔断只认渠道/网络层面的故障：4xx 是坏请求/坏凭据，不该把渠道打死。
func TestChannelFailureClassification(t *testing.T) {
	// 4xx（除 429）：failover 但绝不熔断
	for _, s := range []int{400, 401, 403, 404, 413, 422} {
		if channelFailure(&UpstreamError{Status: s}, s) {
			t.Fatalf("%d must not trip the breaker", s)
		}
	}
	// 429 / 5xx：熔断
	if !channelFailure(&UpstreamError{Status: 429}, 429) {
		t.Fatal("429 must trip the breaker")
	}
	for _, s := range []int{500, 502, 503, 504} {
		if !channelFailure(&UpstreamError{Status: s}, s) {
			t.Fatalf("%d must trip the breaker", s)
		}
	}
	// 传输层错误（连接失败、超时、上游中断读流）
	if !channelFailure(context.DeadlineExceeded, 0) {
		t.Fatal("timeout must trip the breaker")
	}
	if !channelFailure(errors.New("read upstream: connection reset by peer"), 200) {
		t.Fatal("interrupted stream must trip the breaker")
	}
	// 无 error 时退回 shouldFailover 语义
	if channelFailure(nil, 400) || !channelFailure(nil, 503) {
		t.Fatal("status-only classification must follow shouldFailover")
	}
}

// 端到端锁定：上游返回非 2xx 时 upstreamCall 产出的错误必须被正确分类。
// 旧行为把任意 4xx 也当渠道故障，一个被反复重试的坏请求几秒内就能把渠道熔断。
func TestUpstreamErrorClassificationE2E(t *testing.T) {
	h := &Handler{HTTPClient: newUpstreamClient(5 * time.Second)}
	cases := []struct {
		status   int
		wantTrip bool
	}{
		{400, false}, {401, false}, {404, false}, {413, false},
		{429, true}, {500, true}, {503, true},
	}
	for _, tc := range cases {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		}))
		w := httptest.NewRecorder()
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		status, _, err := h.upstreamCall(t.Context(), h.HTTPClient, up.URL, "sk-x", body, false, protoOpenAI, w, "")
		up.Close()
		if err == nil {
			t.Fatalf("upstream %d must produce an error", tc.status)
		}
		if status != tc.status {
			t.Fatalf("upstream %d: returned status %d", tc.status, status)
		}
		if got := channelFailure(err, status); got != tc.wantTrip {
			t.Fatalf("upstream %d: channelFailure=%v, want %v", tc.status, got, tc.wantTrip)
		}
	}
}

// ---- 限流（miniredis 不在依赖里，用真 redis 集成测试跳过逻辑写在 e2e）----

func TestRateLimiterNilSafe(t *testing.T) {
	var rl *RateLimiter
	release, ok := rl.Check(t.Context(), 1)
	if !ok {
		t.Fatal("nil limiter must allow")
	}
	release()
}

// ---- SSE 端到端（httptest 上游 → streamPassthrough）----

func TestStreamPassthroughE2E(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"id":"1","choices":[{"delta":{"content":"he"}}]}`,
		`data: {"id":"1","choices":[{"delta":{"content":"llo"}}]}`,
		`data: {"id":"1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}`,
		`data: [DONE]`,
		"",
	}, "\n")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	}))
	defer up.Close()

	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatalf("upstream get: %v", err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	uResp := resp
	_, usage, err := func() (int, UsageRecord, error) {
		return streamPassthrough(w, uResp)
	}()
	if err != nil {
		t.Fatalf("streamPassthrough: %v", err)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 22 {
		t.Fatalf("usage from last chunk failed: %+v", usage)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"he"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("passthrough body incomplete: %s", body)
	}
}
