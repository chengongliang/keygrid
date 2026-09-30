package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/workbuddy"
)

// workbuddy_scrub_test.go WAF 脱敏在 relay 热路径的接线契约（设计 §3.4）。
//
// 断言三档语义真正作用到"发往上游的字节"：off / 未接线原样、on 改写指纹片段、
// shadow 字节级不变。用 httptest 冒充上游，无 DB、无真实网络。

// fakeScrubGate 只关心 ScrubModeFor 的假 gate；准入/回填 no-op。
type fakeScrubGate struct{ mode string }

func (f *fakeScrubGate) Admit(*model.Provider, string) error { return nil }
func (f *fakeScrubGate) Observe(workbuddy.Event)             {}
func (f *fakeScrubGate) ScrubModeFor(*model.Provider) string { return f.mode }

// scrubUpstreamBody 构造一条带指纹的 chat/completions 请求体（system 消息）。
func scrubUpstreamBody(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "system", "content": text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestApplyWorkbuddyScrubReachesUpstream(t *testing.T) {
	const fingerprint = "You are Claude Code, Anthropic's official CLI for Claude 11128"
	original := scrubUpstreamBody(t, fingerprint)
	prov := &model.Provider{ID: 7, UserID: 3}

	cases := []struct {
		name string
		gate WorkbuddyGate
		// 期望 applyWorkbuddyScrub 返回值是否与原始 body 字节相同
		scrubbedIdentical bool
		// 期望上游收到的 body 是否与原始 body 字节相同
		upstreamIdentical bool
		wantAbsent        []string
		wantPresent       []string
	}{
		{"off 原样发送", &fakeScrubGate{mode: workbuddy.ScrubOff}, true, true, nil, nil},
		{"nil gate 原样发送", nil, true, true, nil, nil},
		{"shadow 字节级不变", &fakeScrubGate{mode: workbuddy.ScrubShadow}, true, true, nil, nil},
		{
			name: "on 改写指纹后再发上游",
			gate: &fakeScrubGate{mode: workbuddy.ScrubOn},
			wantAbsent: []string{
				"Anthropic's official CLI for Claude",
				"11128",
			},
			wantPresent: []string{"11-128", "official CLI tool for Claude"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var received []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer srv.Close()

			h := &Handler{Workbuddy: tc.gate}
			got := h.applyWorkbuddyScrub(prov, original)
			if tc.scrubbedIdentical && string(got) != string(original) {
				t.Fatalf("期望 body 原样返回，得到 %s", got)
			}

			// 把 handler 产出的 body 真正发往假上游，断言上游收到什么。
			rec := httptest.NewRecorder()
			status, _, err := h.upstreamCall(context.Background(), srv.Client(), srv.URL, "tok",
				got, false, protoOpenAI, rec, "", nil, false)
			if err != nil || status != http.StatusOK {
				t.Fatalf("upstreamCall status=%d err=%v", status, err)
			}
			if len(received) == 0 {
				t.Fatal("上游没有收到请求体")
			}
			if tc.upstreamIdentical && string(received) != string(original) {
				t.Fatalf("上游收到的 body 应与原始一致，得到 %s", received)
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(string(received), bad) {
					t.Fatalf("上游 body 不应再含 %q: %s", bad, received)
				}
			}
			for _, want := range tc.wantPresent {
				if !strings.Contains(string(received), want) {
					t.Fatalf("上游 body 应含 %q: %s", want, received)
				}
			}
			// 脱敏只改指纹片段，消息条数与角色必须保留。
			if tc.name == "on 改写指纹后再发上游" {
				var obj map[string]any
				if err := json.Unmarshal(received, &obj); err != nil {
					t.Fatalf("改写后仍是合法 JSON: %v", err)
				}
				msgs, _ := obj["messages"].([]any)
				if len(msgs) != 1 {
					t.Fatalf("消息条数应保留: %s", received)
				}
			}
		})
	}
}

// nil gate 的档位读取必须回落 off（未接线即不启用）。
func TestWorkbuddyScrubModeNilGate(t *testing.T) {
	h := &Handler{}
	if got := h.workbuddyScrubMode(&model.Provider{ID: 1, UserID: 1}); got != workbuddy.ScrubOff {
		t.Fatalf("nil gate = %q, want off", got)
	}
	// gate 返回未知值时归一为 off。
	h.Workbuddy = &fakeScrubGate{mode: "garbage"}
	if got := h.workbuddyScrubMode(&model.Provider{ID: 1, UserID: 1}); got != workbuddy.ScrubOff {
		t.Fatalf("unknown mode = %q, want off", got)
	}
}

// 审计/日志摘要只含规则名与计数（键名排序稳定），不含任何内容。
func TestScrubHitSummary(t *testing.T) {
	got := scrubHitSummary(map[string]int{"error_code_11128": 2, "claude_code_identity": 1})
	if got != "claude_code_identity=1,error_code_11128=2" {
		t.Fatalf("scrubHitSummary = %q", got)
	}
	if scrubHitSummary(nil) != "" {
		t.Fatal("空命中应为空串")
	}
}
