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

	cands := buildCandidates(providers, "gpt-4o")
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
	cands = buildCandidates(narrow, "other-model")
	if len(cands) != 0 {
		t.Fatalf("expected 0 candidates for unmapped model, got %d", len(cands))
	}

	// 全部不可用
	empty := []model.Provider{{ID: 8, Kind: "api_key", Enabled: false}}
	if cands = buildCandidates(empty, "x"); len(cands) != 0 {
		t.Fatal("expected no candidates when no channel enabled")
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

func TestCircuitBreaker(t *testing.T) {
	st := NewState()
	for i := 0; i < defaultFailThreshold; i++ {
		st.OnFailure()
	}
	if state, _ := st.Snapshot(); state != "open" {
		t.Fatalf("expected open after %d failures, got %s", defaultFailThreshold, state)
	}
	if st.Allow() {
		t.Fatal("open breaker must reject")
	}

	// 到期 → half-open，放行 1 个试探
	st.openedAt = st.openedAt.Add(-defaultOpenDuration - time.Second)
	if !st.Allow() {
		t.Fatal("half-open must allow one probe")
	}
	if st.Allow() {
		t.Fatal("half-open must allow only one probe")
	}
	st.OnSuccess()
	if state, _ := st.Snapshot(); state != "closed" {
		t.Fatalf("expected closed after success, got %s", state)
	}

	// half-open 试探失败 → 重新 open
	// 构造：先熔断 → 到期进入 half-open → Allow() 取得试探资格 → 试探失败
	st.OnFailure() // failures=1
	for i := 1; i < defaultFailThreshold; i++ {
		st.OnFailure()
	}
	if state, _ := st.Snapshot(); state != "open" {
		t.Fatalf("precondition: expected open, got %s", state)
	}
	st.openedAt = st.openedAt.Add(-defaultOpenDuration - time.Second)
	if !st.Allow() {
		t.Fatal("half-open must allow probe")
	}
	st.OnFailure() // probe fails → re-trip
	if state, _ := st.Snapshot(); state != "open" {
		t.Fatalf("failed probe must re-trip, got %s", state)
	}
}

func TestBreakerRegistry(t *testing.T) {
	b := NewBreaker()
	for i := 0; i < defaultFailThreshold; i++ {
		b.OnFailure(42)
	}
	if b.Allow(42) {
		t.Fatal("channel 42 should be open")
	}
	if !b.Allow(43) {
		t.Fatal("channel 43 should be independent")
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
