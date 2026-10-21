package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- 模型目录 ----

func TestFetchCatalogParsesModelsAndConfigs(t *testing.T) {
	body := `{"chat":[
		{"key":"auto","display_name":"Auto","enable":true,"max_input_tokens":180000,
		 "max_output_tokens":32768,"is_vl":true,"is_reasoning":false,
		 "context_config":[{"name":"200K","tokenCount":"200K","isDefault":true}]},
		{"key":"qmodel_latest","display_name":"Qwen3.7-Max","max_input_tokens":990000,
		 "is_reasoning":true},
		{"key":"hidden_model","enable":false,"display_name":"Hidden"}
	]}`
	var gotPath, gotSigPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSigPath = r.Header.Get("Cosy-Sigpath")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			t.Errorf("missing cosy authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	rc := Resolved{
		Region:    RegionIntl,
		Token:     "dt-test",
		UserID:    "user-1",
		MachineID: "machine-1",
	}
	// 用测试端点替换区域 bases
	defer swapBases(t, RegionIntl, srv.URL)()

	cat, err := fetchCatalog(context.Background(), srv.Client(), rc)
	if err != nil {
		t.Fatalf("fetchCatalog: %v", err)
	}
	if gotPath != "/algo/api/v2/model/list" {
		t.Errorf("path = %q", gotPath)
	}
	if gotSigPath != "/api/v2/model/list" {
		t.Errorf("sigpath = %q", gotSigPath)
	}
	if len(cat.Models) != 3 {
		t.Fatalf("models = %d, want 3", len(cat.Models))
	}
	byKey := map[string]Model{}
	for _, m := range cat.Models {
		byKey[m.Key] = m
	}
	if m := byKey["auto"]; m.Name != "Auto" || !m.IsVL || m.ContextLength != 180000 {
		t.Errorf("auto model = %+v", m)
	}
	if m := byKey["qmodel_latest"]; !m.IsReasoning || m.ContextLength != 990000 {
		t.Errorf("qmodel_latest = %+v", m)
	}
	if m := byKey["hidden_model"]; !m.Hidden {
		t.Errorf("enable:false should be Hidden: %+v", m)
	}
	if _, ok := cat.Config("auto"); !ok {
		t.Error("auto config missing")
	}
	if _, ok := cat.Config("nope"); ok {
		t.Error("unexpected config for unknown model")
	}
}

func TestCatalogCacheAndForceRefresh(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"chat":[{"key":"auto","max_input_tokens":1000}]}`))
	}))
	defer srv.Close()
	defer swapBases(t, RegionIntl, srv.URL)()

	rt := NewRuntime()
	rc := Resolved{Region: RegionIntl, Token: "dt-x", UserID: "u1"}
	ctx := context.Background()

	if _, err := rt.Catalog(ctx, srv.Client(), rc, false); err != nil {
		t.Fatalf("first catalog: %v", err)
	}
	if _, err := rt.Catalog(ctx, srv.Client(), rc, false); err != nil {
		t.Fatalf("cached catalog: %v", err)
	}
	if calls != 1 {
		t.Errorf("cached call hit upstream %d times, want 1", calls)
	}
	if _, err := rt.Catalog(ctx, srv.Client(), rc, true); err != nil {
		t.Fatalf("force catalog: %v", err)
	}
	if calls != 2 {
		t.Errorf("force refresh should bypass cache, calls = %d", calls)
	}

	// 过期后重新拉取
	rt.SetNow(func() time.Time { return time.Now().Add(2 * time.Hour) })
	if _, err := rt.Catalog(ctx, srv.Client(), rc, false); err != nil {
		t.Fatalf("expired catalog: %v", err)
	}
	if calls != 3 {
		t.Errorf("expired cache should refetch, calls = %d", calls)
	}
}

func TestFetchCatalogUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"101","message":"Signature invalid"}`))
	}))
	defer srv.Close()
	defer swapBases(t, RegionIntl, srv.URL)()

	rc := Resolved{Region: RegionIntl, Token: "dt-x", UserID: "u1"}
	_, err := fetchCatalog(context.Background(), srv.Client(), rc)
	ue, ok := err.(*UpstreamError)
	if !ok || ue.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want UpstreamError 403", err)
	}
}

// ---- PAT / userinfo ----

func TestResolveCredentialPATExchangeAndUserInfo(t *testing.T) {
	var exchangeCalls, userinfoCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			exchangeCalls++
			if got := r.Header.Get("User-Agent"); got != "qodercli/1.0.0" {
				t.Errorf("exchange UA = %q", got)
			}
			_, _ = w.Write([]byte(`{"token":"jt-abc","expires_in":86400}`))
		case "/api/v1/userinfo":
			userinfoCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer jt-abc" {
				t.Errorf("userinfo auth = %q", got)
			}
			_, _ = w.Write([]byte(`{"id":"user-42","name":"Alice","email":"a@example.com"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	defer swapBases(t, RegionIntl, srv.URL)()

	rt := NewRuntime()
	rc, err := rt.ResolveCredential(context.Background(), srv.Client(), RegionIntl, "pt-token", nil)
	if err != nil {
		t.Fatalf("ResolveCredential: %v", err)
	}
	if rc.Token != "jt-abc" || rc.UserID != "user-42" || rc.PAT != "pt-token" {
		t.Fatalf("resolved = %+v", rc)
	}
	// 第二次：PAT 与 userinfo 均应命中缓存
	if _, err := rt.ResolveCredential(context.Background(), srv.Client(), RegionIntl, "pt-token", nil); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if exchangeCalls != 1 || userinfoCalls != 1 {
		t.Errorf("calls = exchange %d / userinfo %d, want 1/1", exchangeCalls, userinfoCalls)
	}
}

func TestResolveCredentialDeviceTokenUsesExtraUserID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("should not call userinfo when extra has userId: %s", r.URL.Path)
	}))
	defer srv.Close()

	rt := NewRuntime()
	rc, err := rt.ResolveCredential(context.Background(), srv.Client(), RegionIntl, "dt-token",
		map[string]string{"userId": "u-9", "machineId": "m-9"})
	if err != nil {
		t.Fatalf("ResolveCredential: %v", err)
	}
	if rc.UserID != "u-9" || rc.MachineID != "m-9" || rc.Token != "dt-token" {
		t.Fatalf("resolved = %+v", rc)
	}
}

func TestResolveCredentialBackfillsUserIDViaUserInfo(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"user_id":"legacy-user"}`))
	}))
	defer srv.Close()
	defer swapBases(t, RegionIntl, srv.URL)()

	rt := NewRuntime()
	// 存量凭据：只有 machineId，没有 userId
	rc, err := rt.ResolveCredential(context.Background(), srv.Client(), RegionIntl, "dt-legacy",
		map[string]string{"machineId": "m"})
	if err != nil {
		t.Fatalf("ResolveCredential: %v", err)
	}
	if rc.UserID != "legacy-user" {
		t.Fatalf("userID = %q", rc.UserID)
	}
	if calls != 1 {
		t.Errorf("userinfo calls = %d", calls)
	}
}

func TestParseExpiryForms(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if got := parseExpiry(float64(1781594470000), nil, now, time.Hour); got.UnixMilli() != 1781594470000 {
		t.Errorf("ms number: %v", got)
	}
	if got := parseExpiry("1781594470000", nil, now, time.Hour); got.UnixMilli() != 1781594470000 {
		t.Errorf("ms string: %v", got)
	}
	if got := parseExpiry("2026-06-16T07:15:04Z", nil, now, time.Hour); got.UTC().Format(time.RFC3339) != "2026-06-16T07:15:04Z" {
		t.Errorf("rfc3339: %v", got)
	}
	if got := parseExpiry(nil, float64(60), now, time.Hour); got.Sub(now) != time.Minute {
		t.Errorf("expires_in: %v", got)
	}
	if got := parseExpiry(nil, nil, now, time.Hour); got.Sub(now) != time.Hour {
		t.Errorf("default: %v", got)
	}
}

// ---- 上下文档位 ----

func TestContextTiersParsing(t *testing.T) {
	cfg := json.RawMessage(`{"max_input_tokens":180000,"context_config":[
		{"name":"200K","tokenCount":"200K","isDefault":true},
		{"name":"400K","tokenCount":400000},
		{"name":"1M","tokenCount":"1M"}
	]}`)
	tiers := ContextTiers(cfg)
	if len(tiers) != 3 {
		t.Fatalf("tiers = %d", len(tiers))
	}
	if tiers[0].TokenCount != 200000 || !tiers[0].IsDefault || tiers[0].Name != "200K" {
		t.Errorf("tier[0] = %+v", tiers[0])
	}
	if tiers[2].TokenCount != 1000000 || tiers[2].Name != "1M" {
		t.Errorf("tier[2] = %+v", tiers[2])
	}
}

func TestResolveTierModes(t *testing.T) {
	cfg := json.RawMessage(`{"max_input_tokens":180000,"context_config":[
		{"name":"200K","tokenCount":"200K","isDefault":true},
		{"name":"400K","tokenCount":"400K"},
		{"name":"1M","tokenCount":"1M"}]}`)
	short := PromptInput{System: "hi", Messages: []any{map[string]any{"role": "user", "content": "hello"}}}

	if got := ResolveTier(cfg, short, "max"); got == nil || got.Tier.TokenCount != 1000000 || got.Reason != "forced:max" {
		t.Errorf("max mode = %+v", got)
	}
	if got := ResolveTier(cfg, short, "default"); got == nil || got.Tier.TokenCount != 200000 {
		t.Errorf("default mode = %+v", got)
	}
	if got := ResolveTier(cfg, short, "400K"); got == nil || got.Tier.TokenCount != 400000 {
		t.Errorf("named mode = %+v", got)
	}
	// auto + 短提示：默认档装得下 → 不改动 payload
	if got := ResolveTier(cfg, short, ""); got != nil {
		t.Errorf("auto with short prompt should return nil, got %+v", got)
	}
	// auto + 超大 prompt：升到最大档
	huge := PromptInput{System: strings.Repeat("x", 8_000_000)}
	got := ResolveTier(cfg, huge, "")
	if got == nil || got.Tier.TokenCount != 1000000 || got.Reason != "auto:largest" {
		t.Errorf("auto escalation = %+v", got)
	}
}

func TestEstimatePromptTokensCountsCJK(t *testing.T) {
	latin := EstimatePromptTokens(PromptInput{System: strings.Repeat("a", 400)})
	cjk := EstimatePromptTokens(PromptInput{System: strings.Repeat("中", 400)})
	if cjk < latin*3 {
		t.Errorf("CJK estimate should be far larger: cjk=%d latin=%d", cjk, latin)
	}
}

func TestApplyTierWritesIDEFields(t *testing.T) {
	payload := map[string]any{
		"parameters":   map[string]any{"max_tokens": 100},
		"chat_context": map[string]any{"extra": map[string]any{}},
		"model_config": map[string]any{"max_input_tokens": 180000},
	}
	ApplyTier(payload, Tier{Name: "1M", TokenCount: 1000000})
	if got := payload["parameters"].(map[string]any)["context_length"]; got != 1000000 {
		t.Errorf("context_length = %v", got)
	}
	extra := payload["chat_context"].(map[string]any)["extra"].(map[string]any)
	override := extra["ideModelConfigOverride"].(map[string]any)
	if override["max_input_tokens"] != 1000000 {
		t.Errorf("override = %v", override)
	}
	if payload["model_config"].(map[string]any)["max_input_tokens"] != 1000000 {
		t.Errorf("model_config not updated")
	}
}

// ---- 请求体构造 ----

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	raw := `{"chat":[{"key":"auto","display_name":"Auto","max_input_tokens":180000,
		"max_output_tokens":8192,"is_reasoning":false,"source":"system",
		"context_config":[{"name":"200K","tokenCount":"200K","isDefault":true}]},
		{"key":"qmodel_latest","display_name":"Qwen3.7-Max","max_input_tokens":990000,
		"is_reasoning":true}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(raw))
	}))
	t.Cleanup(srv.Close)
	cat, err := fetchCatalog(context.Background(), srv.Client(), Resolved{Region: RegionIntl, Token: "dt", UserID: "u"})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return cat
}

func TestBuildRequestExtractsSystemAndKeepsImages(t *testing.T) {
	cat := testCatalog(t)
	body := `{
		"model":"auto",
		"max_tokens":4000,
		"messages":[
			{"role":"system","content":"you are helpful"},
			{"role":"system","content":[{"type":"text","text":"second system"}]},
			{"role":"user","content":" hi "},
			{"role":"user","content":[
				{"type":"text","text":"see this"},
				{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}
			]},
			{"role":"tool","tool_call_id":"c1","content":"tool result"}
		],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]
	}`
	built, err := BuildRequest(context.Background(), BuildInput{
		OpenAIBody: []byte(body),
		ModelKey:   "auto",
		UserID:     "user-1",
		Catalog:    cat,
	}, Resolved{Region: RegionIntl, Token: "dt-x", UserID: "user-1"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(built.Payload, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload["system"] != "you are helpful\n\nsecond system" {
		t.Errorf("system = %q", payload["system"])
	}
	if payload["stream"] != true || payload["session_type"] != "qodercli" || payload["agent_id"] != "agent_common" {
		t.Errorf("envelope fields wrong: %v", payload)
	}
	msgs := payload["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (system hoisted)", len(msgs))
	}
	if content := msgs[0].(map[string]any)["content"]; content != " hi " {
		t.Errorf("plain text content should stay string, got %#v", content)
	}
	blocks, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("image content should stay blocks: %#v", msgs[1])
	}
	if blocks[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("image block lost: %#v", blocks[1])
	}
	params := payload["parameters"].(map[string]any)
	if params["max_tokens"] != float64(4000) {
		t.Errorf("max_tokens = %v (request cap should win)", params["max_tokens"])
	}
	cfg := payload["model_config"].(map[string]any)
	if cfg["key"] != "auto" {
		t.Errorf("model_config = %v", cfg)
	}
	if built.ModelSource != "system" || built.ModelKey != "auto" {
		t.Errorf("built meta = %+v", built)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools = %v", tools)
	}
}

func TestBuildRequestRequiresModelConfig(t *testing.T) {
	cat := testCatalog(t)
	_, err := BuildRequest(context.Background(), BuildInput{
		OpenAIBody: []byte(`{"model":"unknown","messages":[{"role":"user","content":"hi"}]}`),
		ModelKey:   "unknown",
		Catalog:    cat,
	}, Resolved{Region: RegionIntl, Token: "dt", UserID: "u"})
	if err == nil || !strings.Contains(err.Error(), "model_config") {
		t.Fatalf("err = %v, want model_config error", err)
	}
}

func TestBuildRequestSessionAndRecordIDsAreStable(t *testing.T) {
	cat := testCatalog(t)
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	rc := Resolved{Region: RegionIntl, Token: "dt", UserID: "u1"}
	a, err := BuildRequest(context.Background(), BuildInput{OpenAIBody: body, ModelKey: "auto", UserID: "u1", Catalog: cat}, rc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildRequest(context.Background(), BuildInput{OpenAIBody: body, ModelKey: "auto", UserID: "u1", Catalog: cat}, rc)
	if err != nil {
		t.Fatal(err)
	}
	var pa, pb map[string]any
	_ = json.Unmarshal(a.Payload, &pa)
	_ = json.Unmarshal(b.Payload, &pb)
	if pa["session_id"] != pb["session_id"] {
		t.Error("session_id should be stable for same user+model")
	}
	if pa["chat_record_id"] != pb["chat_record_id"] {
		t.Error("chat_record_id should be stable for same content")
	}
	if pa["request_id"] == pb["request_id"] {
		t.Error("request_id should be unique per request")
	}
}

func TestNormalizeContentFlattensTextOnly(t *testing.T) {
	got := normalizeContent([]any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "text", "text": "b"},
	})
	if got != "a\nb" {
		t.Errorf("text-only content = %#v", got)
	}
	got = normalizeContent([]any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}},
		map[string]any{"type": "text", "text": "b"},
	})
	blocks, ok := got.([]any)
	if !ok || len(blocks) != 3 {
		t.Fatalf("mixed content = %#v", got)
	}
	if blocks[0].(map[string]any)["text"] != "a" || blocks[2].(map[string]any)["text"] != "b" {
		t.Errorf("text ordering broken: %#v", blocks)
	}
}

func TestNormalizeContentStubsFileAndDocument(t *testing.T) {
	got := normalizeContent([]any{
		map[string]any{"type": "file", "file": map[string]any{"filename": "a.pdf"}},
		map[string]any{"type": "document", "title": "b.pdf", "source": map[string]any{"type": "base64", "data": "x"}},
	})
	s, ok := got.(string)
	if !ok || !strings.Contains(s, "a.pdf") || !strings.Contains(s, "b.pdf") {
		t.Fatalf("stub text = %#v", got)
	}
}

// ---- SSE 拆封 ----

func sseBody(lines ...string) string {
	out := ""
	for _, l := range lines {
		out += "data: " + l + "\n\n"
	}
	return out
}

func envelopeChunk(inner string) string {
	payload, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	return string(payload)
}

func TestStreamUnwrapsEnvelopeAndCoalescesUsage(t *testing.T) {
	upstream := sseBody(
		envelopeChunk(`{"id":"c1","model":"auto","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`),
		envelopeChunk(`{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"content":"lo"}}]}`),
		envelopeChunk(`{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"finish_reason":"stop"}}]}`),
		envelopeChunk(`{"id":"c1","model":"auto","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		"[DONE]",
		// 上游发完 [DONE] 后仍挂着 keepalive：这里必须已被丢弃
		envelopeChunk(`{"id":"c1","choices":[{"delta":{"content":"late"}}]}`),
	)
	st := NewStream(strings.NewReader(upstream), nil, "auto")
	if err := st.Peek(); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	out := readAll(t, st)

	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Errorf("content chunks missing:\n%s", out)
	}
	if strings.Contains(out, "late") {
		t.Errorf("keepalive after terminal should be dropped:\n%s", out)
	}
	if strings.Count(out, "[DONE]") != 1 {
		t.Errorf("[DONE] count = %d:\n%s", strings.Count(out, "[DONE]"), out)
	}
	// 终端 chunk 必须同时带 finish_reason 与 usage
	var terminal map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "data: [DONE]") {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &chunk) != nil {
			continue
		}
		if _, ok := chunk["usage"]; ok {
			terminal = chunk
		}
	}
	if terminal == nil {
		t.Fatalf("no usage-bearing terminal chunk:\n%s", out)
	}
	choice := terminal["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("terminal finish_reason = %v", choice["finish_reason"])
	}
	usage := terminal["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["total_tokens"] != float64(15) {
		t.Errorf("usage = %v", usage)
	}
}

func TestStreamFirstFrameErrorBecomesUpstreamError(t *testing.T) {
	body := sseBody(string(`{"statusCodeValue":403,"body":"{\"code\":\"110\",\"message\":\"billing block\"}"}`))
	st := NewStream(strings.NewReader(body), nil, "auto")
	err := st.Peek()
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("err = %v, want *UpstreamError", err)
	}
	if ue.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (billing)", ue.Status)
	}
	if !strings.Contains(ue.Body, "billing block") {
		t.Errorf("body = %q", ue.Body)
	}
}

func TestStreamMidStreamErrorEmitsErrorChunk(t *testing.T) {
	body := sseBody(
		envelopeChunk(`{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`),
		string(`{"statusCodeValue":500,"body":"upstream boom"}`),
	)
	st := NewStream(strings.NewReader(body), nil, "auto")
	if err := st.Peek(); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	out := readAll(t, st)
	if !strings.Contains(out, "[qoder error 500") {
		t.Errorf("missing synthesized error chunk:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "[DONE]") {
		t.Errorf("stream should terminate with [DONE]:\n%s", out)
	}
}

func TestStreamRawOpenAIChunkPassThrough(t *testing.T) {
	// 上游若直接吐 OpenAI chunk（非信封），也要能透传
	raw := `{"id":"c1","choices":[{"index":0,"delta":{"content":"x"}}]}`
	st := NewStream(strings.NewReader(sseBody(raw)), nil, "auto")
	if err := st.Peek(); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	out := readAll(t, st)
	if !strings.Contains(out, `"content":"x"`) {
		t.Errorf("raw chunk lost:\n%s", out)
	}
}

func TestIsBillingBlock(t *testing.T) {
	cases := map[string]bool{
		`{"code":"110"}`:   true,
		`{"code":112}`:     true,
		`{"code":"10605"}`: true,
		`{"message":"see pricingUrl for details"}`: true,
		`{"code":"500","message":"internal"}`:      false,
		``:                                         false,
	}
	for in, want := range cases {
		if got := isBillingBlock(in); got != want {
			t.Errorf("isBillingBlock(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCanonicalizeUsageAliases(t *testing.T) {
	got := canonicalizeUsage(map[string]any{
		"input_tokens":            float64(7),
		"output_tokens":           float64(3),
		"cache_read_input_tokens": float64(2),
	})
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("usage = %#v", got)
	}
	if m["prompt_tokens"] != 7 || m["completion_tokens"] != 3 || m["total_tokens"] != 10 {
		t.Errorf("usage = %v", m)
	}
	if m["cached_tokens"] != 2 {
		t.Errorf("cached_tokens = %v", m["cached_tokens"])
	}
}

// ---- 附件 ----

func TestParseDataURI(t *testing.T) {
	d, ok := parseDataURI("data:image/png;base64,AAAA")
	if !ok || d.mimeType != "image/png" || d.base64 != "AAAA" {
		t.Fatalf("parsed = %+v ok=%v", d, ok)
	}
	if _, ok := parseDataURI("https://example.com/a.png"); ok {
		t.Error("http url should not parse as data URI")
	}
}

func TestRewriteAttachmentsUploadsImages(t *testing.T) {
	var uploads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		if r.Method != http.MethodPut || r.URL.Path != "/algo/api/v2/image/upload" {
			t.Errorf("unexpected upload request: %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			t.Error("upload missing cosy auth")
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
			t.Errorf("content-type = %q", ct)
		}
		_, _ = w.Write([]byte(`{"result":{"imageUrls":["https://oss.example.com/up.png"]}}`))
	}))
	defer srv.Close()
	defer swapBases(t, RegionIntl, srv.URL)()

	rc := Resolved{Region: RegionIntl, Token: "dt-x", UserID: "u1", MachineID: "m1"}
	b64 := "aGVsbG8=" // "hello"
	messages := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + b64}},
		},
	}}
	out := RewriteAttachments(context.Background(), messages, rc, HTTPUploader{Client: srv.Client(), RC: rc})
	blocks := out[0].(map[string]any)["content"].([]any)
	url := blocks[0].(map[string]any)["image_url"].(map[string]any)["url"]
	if url != "https://oss.example.com/up.png" {
		t.Fatalf("image url = %v", url)
	}
	if uploads != 1 {
		t.Errorf("uploads = %d", uploads)
	}
}

func TestRewriteAttachmentsStubsHugeFiles(t *testing.T) {
	rc := Resolved{Region: RegionIntl, Token: "dt-x", UserID: "u1"}
	messages := []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "file", "file": map[string]any{
				"filename":  "big.pdf",
				"file_data": "data:application/pdf;base64,AAAA",
			}},
		},
	}}
	out := RewriteAttachments(context.Background(), messages, rc, nil)
	blocks := out[0].(map[string]any)["content"].([]any)
	text, _ := blocks[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "big.pdf") || !strings.Contains(text, "file omitted") {
		t.Fatalf("file block = %#v", blocks[0])
	}
}

func TestExtractUploadURLShapes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"url":"https://a/1.png"}`, "https://a/1.png"},
		{`{"result":{"imageUrl":"https://a/2.png"}}`, "https://a/2.png"},
		{`{"result":{"imageUrls":["https://a/3.png"]}}`, "https://a/3.png"},
		{`{"body":"{\"ossUrl\":\"https://a/4.png\"}"}`, "https://a/4.png"},
		{`{"nope":1}`, ""},
	}
	for _, c := range cases {
		var v any
		if err := json.Unmarshal([]byte(c.in), &v); err != nil {
			t.Fatalf("bad case %s", c.in)
		}
		if got := extractUploadURL(v, 0); got != c.want {
			t.Errorf("extractUploadURL(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- 测试助手 ----

// readAll 读干 Stream。
func readAll(t *testing.T, r interface{ Read([]byte) (int, error) }) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String()
}

// swapBases 把区域端点临时指向测试服务器，返回恢复函数。
func swapBases(t *testing.T, region, base string) func() {
	t.Helper()
	orig, ok := regionBases[region]
	if !ok {
		t.Fatalf("unknown region %s", region)
	}
	parts := strings.SplitN(base, "://", 2)
	if len(parts) != 2 {
		t.Fatalf("bad base %s", base)
	}
	regionBases[region] = bases{
		Chat:    base,
		ChatAlt: base,
		OpenAPI: base,
		Center:  base,
		Login:   base + "/device/selectAccounts",
		Website: base,
	}
	return func() { regionBases[region] = orig }
}
