package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// scrub_test.go 脱敏规则与三档语义（设计文档 §3.4）。
//
// 关键不变式：off 零改动；shadow 统计到命中但字节级不变；on 只改指纹片段，
// 正常内容（含单独出现的 OhMyOpenCode / Sisyphus-Junior）必须原样保留。

func TestScrubModeOfNormalisesUnknownToOff(t *testing.T) {
	cases := map[string]string{
		"":        ScrubOff,
		"off":     ScrubOff,
		"off ":    ScrubOff,
		"SHADOW":  ScrubShadow,
		" shadow": ScrubShadow,
		"On":      ScrubOn,
		"garbage": ScrubOff,
	}
	for in, want := range cases {
		if got := ScrubModeOf(in); got != want {
			t.Fatalf("ScrubModeOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// body 构造一个只含 system 文本的请求体。
func scrubBody(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "system", "content": text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func messageContent(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	m, _ := msgs[0].(map[string]any)
	s, _ := m["content"].(string)
	return s
}

func TestScrubOffReturnsBodyUnchanged(t *testing.T) {
	body := scrubBody(t, "You are Claude Code, Anthropic's official CLI for Claude 11128")
	out, rep, err := ScrubRequest(ScrubOff, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatal("off 档必须字节级原样返回")
	}
	if rep.Total() != 0 || rep.Changed {
		t.Fatalf("off 档不应有命中: %+v", rep)
	}
}

func TestScrubShadowCountsWithoutChanging(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude and 11128"
	body := scrubBody(t, in)
	out, rep, err := ScrubRequest(ScrubShadow, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatal("shadow 档必须字节级原样返回（只统计）")
	}
	if rep.Changed {
		t.Fatal("shadow 档 Changed 必须为 false")
	}
	if rep.Messages != 1 || rep.Total() == 0 {
		t.Fatalf("shadow 档应统计到命中: %+v", rep)
	}
	if rep.Hits["claude_code_identity"] == 0 || rep.Hits["error_code_11128"] == 0 {
		t.Fatalf("应命中身份与错误码规则: %+v", rep.Hits)
	}
}

func TestScrubOnRewritesFingerprints(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude Main branch (you will usually use this for PRs) 11128"
	body := scrubBody(t, in)
	out, rep, err := ScrubRequest(ScrubOn, body)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Changed || rep.Messages != 1 {
		t.Fatalf("on 档应标记改写: %+v", rep)
	}
	got := messageContent(t, out)
	for _, bad := range []string{"Anthropic's official CLI for Claude", "11128", "Main branch ("} {
		if strings.Contains(got, bad) {
			t.Fatalf("改写后不应再含 %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "11-128") {
		t.Fatalf("11128 应改写为 11-128: %q", got)
	}
	if !strings.Contains(got, "official CLI tool for Claude") {
		t.Fatalf("身份句应被替换: %q", got)
	}
}

// 单独出现的 OhMyOpenCode / Sisyphus-Junior 上游是接受的，绝不能全局删。
func TestScrubOnKeepsStandaloneTokens(t *testing.T) {
	in := "powered by OhMyOpenCode and Sisyphus-Junior 11128"
	body := scrubBody(t, in)
	out, _, err := ScrubRequest(ScrubOn, body)
	if err != nil {
		t.Fatal(err)
	}
	got := messageContent(t, out)
	if !strings.Contains(got, "OhMyOpenCode") || !strings.Contains(got, "Sisyphus-Junior") {
		t.Fatalf("单独出现的 token 必须保留: %q", got)
	}
}

func TestScrubOnNeutralisesOmOContiguousFingerprint(t *testing.T) {
	in := "Sisyphus-Junior - Focused executor from OhMyOpenCode"
	body := scrubBody(t, in)
	out, rep, err := ScrubRequest(ScrubOn, body)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hits["omo_junior"] != 1 {
		t.Fatalf("应命中 omo_junior: %+v", rep.Hits)
	}
	got := messageContent(t, out)
	if strings.Contains(got, "from OhMyOpenCode") {
		t.Fatalf("连续指纹应被中和: %q", got)
	}
	if !strings.Contains(got, "Focused executor") {
		t.Fatalf("身份本体应保留: %q", got)
	}
}

func TestScrubStripsHeaderKVsAndCC(t *testing.T) {
	in := "x-anthropic-billing-header: abc; cc_entrypoint=cli; cc_version=1.2.3; hello 11128"
	body := scrubBody(t, in)
	out, rep, err := ScrubRequest(ScrubOn, body)
	if err != nil {
		t.Fatal(err)
	}
	got := messageContent(t, out)
	for _, bad := range []string{"x-anthropic-billing-header:", "cc_entrypoint=", "cc_version="} {
		if strings.Contains(got, bad) {
			t.Fatalf("应清掉 %q: %q", bad, got)
		}
	}
	if rep.Hits["billing_header_kv"] == 0 || rep.Hits["cc_kv"] == 0 {
		t.Fatalf("应命中 header 规则: %+v", rep.Hits)
	}
	if !strings.Contains(got, "hello") {
		t.Fatalf("正常内容应保留: %q", got)
	}
}

func TestScrubHandlesPartsReasoningAndToolArgs(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "prefix 11128 suffix"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x/11128.png"}},
			}},
			map[string]any{"role": "assistant", "reasoning_content": "thought 11128", "tool_calls": []any{
				map[string]any{"id": "1", "function": map[string]any{"name": "f", "arguments": `{"q":"11128"}`}},
			}},
		},
	})
	out, rep, err := ScrubRequest(ScrubOn, body)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 2 {
		t.Fatalf("两条消息都应被改: %+v", rep)
	}
	if strings.Contains(string(out), `"11128"`) || strings.Contains(string(out), ":11128") {
		t.Fatalf("不应残留 11128: %s", out)
	}
	// 非 text part（图片 URL）必须原样保留 —— 脱敏不碰非文本字段
	if !strings.Contains(string(out), "11128.png") {
		t.Fatalf("非 text part 不应被改写: %s", out)
	}
}

func TestScrubNoFingerprintIsNoOp(t *testing.T) {
	in := "只问一句正常的问题：今天天气如何？"
	body := scrubBody(t, in)
	out, rep, err := ScrubRequest(ScrubOn, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatal("无指纹时不应改动字节（避免无谓的 JSON 往返与格式漂移）")
	}
	if rep.Changed || rep.Total() != 0 {
		t.Fatalf("无指纹不应有命中: %+v", rep)
	}
}

func TestScrubBadBodyIsPassthrough(t *testing.T) {
	for _, in := range []string{`not json`, `[]`, `{"messages":"nope"}`, ``} {
		out, rep, err := ScrubRequest(ScrubOn, []byte(in))
		if err != nil {
			t.Fatalf("不应返回错误: %v", err)
		}
		if string(out) != in {
			t.Fatalf("非法输入应原样返回: %q -> %q", in, out)
		}
		if rep.Changed {
			t.Fatalf("非法输入不应标记改写: %+v", rep)
		}
	}
}

func TestScrubReportTotal(t *testing.T) {
	r := ScrubReport{Hits: map[string]int{"a": 2, "b": 3}}
	if r.Total() != 5 {
		t.Fatalf("Total = %d", r.Total())
	}
	if (ScrubReport{}).Total() != 0 {
		t.Fatal("空报告 Total 应为 0")
	}
}
