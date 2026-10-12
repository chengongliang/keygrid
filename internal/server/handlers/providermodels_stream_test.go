package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/workbuddy"
)

// providermodels_stream_test.go 流式测活 SSE 推送：delta/done 事件序列、
// usage/finish_reason 归账、Codex responses SSE 增量回调。DB 依赖由本地 E2E 覆盖。

// sseFrames 解析 handler 推出的 SSE 帧序列（data: {...}\n\n）。
func sseFrames(t *testing.T, body string) []streamSSEEvent {
	t.Helper()
	var frames []streamSSEEvent
	for _, chunk := range strings.Split(body, "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		if !strings.HasPrefix(chunk, "data: ") {
			t.Fatalf("unexpected sse frame: %q", chunk)
		}
		var ev streamSSEEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(chunk, "data: ")), &ev); err != nil {
			t.Fatalf("parse frame: %v (%s)", err, chunk)
		}
		frames = append(frames, ev)
	}
	return frames
}

func TestPushStreamChat_DeltaAndDone(t *testing.T) {
	// 模拟上游 chat.completions 流：2 帧内容 + 2 帧思考 + usage + finish_reason
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"思考A"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"思考B"}}]}`,
		`data: {"choices":[{"delta":{"content":"def "}}]}`,
		`data: {"choices":[{"delta":{"content":"快速排序"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":24,"completion_tokens":1306}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	rec := httptest.NewRecorder()
	p := &model.Provider{Kind: "api_key", Protocol: "openai", Name: "my-gateway"}
	pushStreamChat(rec, rec, context.Background(), p, strings.NewReader(upstream), time.Now(), "qwen3.8-flash")

	frames := sseFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatal("expect frames")
	}
	// 除最后一帧 done 外，其余都是 delta；done 只出现一次且在末尾
	var content, reasoning strings.Builder
	for i, ev := range frames {
		switch ev.Type {
		case "delta":
			if i == len(frames)-1 {
				t.Fatalf("delta must not be the last frame: %+v", frames)
			}
			if ev.ContentDelta != "" && ev.ReasoningDelta != "" {
				t.Fatalf("one frame should carry a single kind of delta: %+v", ev)
			}
			content.WriteString(ev.ContentDelta)
			reasoning.WriteString(ev.ReasoningDelta)
		case "done":
			if i != len(frames)-1 {
				t.Fatalf("done must be the last frame: %+v", frames)
			}
			if !ev.OK || ev.Error != "" {
				t.Fatalf("done should be ok: %+v", ev)
			}
			if ev.FinishReason != "stop" || ev.PromptTokens != 24 || ev.CompletionTokens != 1306 {
				t.Fatalf("done stats: %+v", ev)
			}
			if ev.Model != "qwen3.8-flash" || !ev.Stream || ev.Channel != "my-gateway" {
				t.Fatalf("done meta: %+v", ev)
			}
			if ev.FirstTokenMs < 0 || ev.LatencyMs < 0 {
				t.Fatalf("done latency: %+v", ev)
			}
		default:
			t.Fatalf("unexpected event type %q", ev.Type)
		}
	}
	if content.String() != "def 快速排序" {
		t.Fatalf("content deltas: %q", content.String())
	}
	if reasoning.String() != "思考A思考B" {
		t.Fatalf("reasoning deltas: %q", reasoning.String())
	}
}

func TestPushStreamChat_StreamInterrupted(t *testing.T) {
	// 上游流异常（超长行触发 scanner 报错，模拟连接坏损/流被截断）：
	// 仍要推 done 收尾，但 ok=false 且带错误信息
	upstream := `data: {"choices":[{"delta":{"content":"half"}}]}` + "\n\n" + strings.Repeat("x", 2<<20)

	rec := httptest.NewRecorder()
	p := &model.Provider{Kind: "api_key", Protocol: "openai"}
	pushStreamChat(rec, rec, context.Background(), p, strings.NewReader(upstream), time.Now(), "m")

	frames := sseFrames(t, rec.Body.String())
	if len(frames) < 2 {
		t.Fatalf("expect delta + done, got %d frames", len(frames))
	}
	done := frames[len(frames)-1]
	if done.Type != "done" || done.OK || done.Error == "" {
		t.Fatalf("interrupted stream should emit failed done: %+v", done)
	}
	if frames[0].Type != "delta" || frames[0].ContentDelta != "half" {
		t.Fatalf("delta before failure: %+v", frames[0])
	}
}

func TestAggregateCodexStreamOnDelta(t *testing.T) {
	// responses SSE：思考增量 + 内容增量 + completed usage；回调增量拼接 = 聚合结果
	upstream := strings.Join([]string{
		`data: {"type":"response.reasoning_summary_text.delta","delta":"先想"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"一步"}`,
		`data: {"type":"response.output_text.delta","delta":"def foo"}`,
		`data: {"type":"response.output_text.delta","delta":"(): pass"}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":42}}}`,
		"",
	}, "\n\n")

	var content, reasoning strings.Builder
	agg, err := relay.AggregateCodexStreamOnDelta(strings.NewReader(upstream), func(c, r string) {
		content.WriteString(c)
		reasoning.WriteString(r)
	})
	if err != nil {
		t.Fatal(err)
	}
	if agg.Content != "def foo(): pass" {
		t.Fatalf("agg content: %q", agg.Content)
	}
	// reasoning 多帧之间由 relay 聚合层插入 "\n" 分隔，增量原样透传
	if agg.Reasoning != "先想\n一步" {
		t.Fatalf("agg reasoning: %q", agg.Reasoning)
	}
	if content.String() != agg.Content || reasoning.String() != agg.Reasoning {
		t.Fatalf("delta replay mismatch: content=%q reasoning=%q", content.String(), reasoning.String())
	}
	if agg.PromptTokens != 7 || agg.CompletionTokens != 42 || agg.FinishReason != "stop" {
		t.Fatalf("agg stats: %+v", agg)
	}

	// 无回调版本等价 AggregateCodexStream
	plain, err := relay.AggregateCodexStream(strings.NewReader(upstream))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Content != agg.Content || plain.CompletionTokens != agg.CompletionTokens {
		t.Fatalf("no-callback agg differs: %+v vs %+v", plain, agg)
	}
}

func TestPushStreamChat_ClientAbort(t *testing.T) {
	// 客户端取消后：不再推 delta，仍尽力推 done 收尾
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"a"}}]}`,
		`data: {"choices":[{"delta":{"content":"b"}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	rec := httptest.NewRecorder()
	p := &model.Provider{Kind: "api_key", Protocol: "openai"}
	pushStreamChat(rec, rec, ctx, p, strings.NewReader(upstream), time.Now(), "m")

	frames := sseFrames(t, rec.Body.String())
	var deltas, dones int
	for _, ev := range frames {
		switch ev.Type {
		case "delta":
			deltas++
		case "done":
			dones++
		}
	}
	if deltas != 0 {
		t.Fatalf("no delta should be pushed after abort, got %d", deltas)
	}
	if dones != 1 {
		t.Fatalf("expect exactly one done frame, got %d", dones)
	}
}

// TestWorkBuddyModelsFallsBackToSnapshot WorkBuddy/CodeBuddy 渠道的模型目录不能走
// /v1/models（GET 会 404 "404 Route Not Found"），必须走 workbuddy 目录四段回落；
// 且**不需要凭据** —— 刚建好还没授权的渠道（Op 为 nil / 无 credential）也要能拉到目录。
//
// 这里直接调用分支函数，避免经 ssrf 校验（那会引入 DNS 依赖，测试要离线可跑）。
func TestWorkBuddyModelsFallsBackToSnapshot(t *testing.T) {
	h := &TestHandler{} // Op 为 nil：模拟「无凭据来源」的新渠道
	client := &http.Client{Timeout: time.Second}
	p := &model.Provider{
		ID:            0, // 未落库
		Name:          "workbuddy-intl",
		OAuthProvider: "workbuddy-intl",
		BaseURL:       "https://www.workbuddy.ai",
	}
	if !workbuddy.IsWorkBuddy(p) {
		t.Fatal("该渠道应被判定为 workbuddy")
	}
	models := h.workBuddyModels(context.Background(), client, p)
	if len(models) == 0 {
		t.Fatal("应回落到内嵌目录（无凭据也要有模型）")
	}
	// 内嵌快照里的 intl 模型应包含这几个
	for _, want := range []string{"deepseek-v4.1-flash", "glm-5.3"} {
		found := false
		for _, m := range models {
			if m == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("内嵌目录缺少 %s: %v", want, models)
		}
	}
}

// TestBuildUpstreamChatProbeReq_WorkBuddy 渠道探活的请求必须走 WorkBuddy 上游协议：
// /v2/chat/completions + IDE 身分头 + 强制流式（否则用户实测会拿到
// {"error_msg":"404 Route Not Found"} —— 通用路径打的是 /v1/chat/completions）。
func TestBuildUpstreamChatProbeReq_WorkBuddy(t *testing.T) {
	p := &model.Provider{ID: 1, Name: "codebuddy-cn", OAuthProvider: "codebuddy-cn", BaseURL: "https://copilot.tencent.com"}
	tok := &oauth.TokenSet{AccessToken: "at-1", Extra: map[string]string{"uid": "uid-1"}}

	req, err := buildUpstreamChatProbeReq(context.Background(), p, "at-1", "", "hy3", "ping", 16, false, tok)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, want := req.URL.String(), "https://copilot.tencent.com/v2/chat/completions"; got != want {
		t.Fatalf("target = %q, want %q", got, want)
	}
	for _, h := range []string{"X-Ide-Type", "X-Ide-Name", "X-CodeBuddy-Request"} {
		if req.Header.Get(h) == "" {
			t.Fatalf("缺少身分头 %s", h)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-1" {
		t.Fatalf("Authorization = %q", got)
	}
	body, _ := io.ReadAll(req.Body)
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent["stream"] != true {
		t.Fatalf("上游只接受流式，必须强制 stream=true: %v", sent["stream"])
	}
	if _, ok := sent["stream_options"]; !ok {
		t.Fatalf("应带 stream_options.include_usage: %v", sent)
	}
}

// TestProbeUpstreamChat_WorkBuddyNonStreamCollapses 上游强制流式时，
// 非流式探活应在本地折叠 SSE 后给出内容，而不是报「解析失败」。
func TestProbeUpstreamChat_WorkBuddyNonStreamCollapses(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	p := &model.Provider{ID: 2, OAuthProvider: "workbuddy-intl", BaseURL: up.URL}
	pr := probeUpstreamChat(context.Background(), up.Client(), p, "at-2", "", "deepseek-v4.1-flash", "ping", 16, false,
		&oauth.TokenSet{AccessToken: "at-2"})
	if pr.Status != 200 {
		t.Fatalf("status = %d err = %q", pr.Status, pr.Err)
	}
	if pr.Content != "pong" {
		t.Fatalf("content = %q（应折叠 SSE 得到 pong）err = %q", pr.Content, pr.Err)
	}
}
