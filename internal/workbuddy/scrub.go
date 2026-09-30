package workbuddy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// scrub.go WAF 指纹脱敏（设计文档 §3.4，默认关闭）。
//
// 背景：上游对「非官方客户端」的识别会把某些字符串当作未授权渠道指纹，命中就回
// 11128 illegal API invocation from an unapproved channel。wb2a 的做法是把请求体
// 里这些**签名片段**改写掉（wb_proxy.py:2365-2466）。
//
// 这等于改写用户的消息内容，因此 KeyGrid 侧的约束是硬的（缺一不合并）：
//   - 三档 off / shadow / on，默认 off；shadow 只统计命中不改写；
//   - on 档每次命中都写审计（事件 workbuddy.scrub，只记规则名与消息条数，不记内容）；
//   - 改写范围刻意收窄：只改 wb2a A/B 验证过的连续指纹片段，不整段删词。
//
// 本文件只做纯文本变换，不碰 HTTP、不碰 DB、不写日志。

// scrubRule 一条改写规则（命中名用于审计，绝不记内容）。
type scrubRule struct {
	Name    string
	Pattern *regexp.Regexp
	Replace string
	// Pair 精确子串替换（与 Pattern 二选一）。
	Pair [2]string
}

// scrubFeatures 命中探测片段（对应 wb_proxy.py:2369-2386 的 SANITIZE_FEATURES）。
// 只要出现任一即认为该文本带指纹，值得走一遍改写。
var scrubFeatures = []string{
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
}

// scrubRules 改写规则，顺序敏感（对齐 wb_proxy.py:2413-2427）：
//  1. 先中和唯一被 A/B 验证过的连续 OmO 指纹 —— 刻意不全局删 "OhMyOpenCode"
//     或 "Sisyphus-Junior" 单词，因为单独出现上游是接受的；
//  2. 再做几个精确句子替换；
//  3. 再清 header 形态与 cc_* kv 片段；
//  4. 最后兜掉裸 header 名。
var scrubRules = []scrubRule{
	{
		Name:    "omo_junior",
		Pattern: regexp.MustCompile(`(?i)Sisyphus-Junior - Focused executor from OhMyOpenCode`),
		Replace: "Sisyphus-Junior - Focused executor",
	},
	{Name: "claude_code_identity", Pair: [2]string{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	}},
	{Name: "main_branch_hint", Pair: [2]string{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	}},
	{Name: "codex_identity", Pair: [2]string{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	}},
	{Name: "anthropic_feedback_url", Pair: [2]string{
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	}},
	{Name: "error_code_11128", Pair: [2]string{"11128", "11-128"}},
	{Name: "billing_header_kv", Pattern: regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\r\n]*;?[ \t]*`), Replace: ""},
	{Name: "cc_kv", Pattern: regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\r\n]*;?[ \t]*`), Replace: ""},
	{Name: "billing_header_bare", Pattern: regexp.MustCompile(`(?i)x-anthropic-billing-header`), Replace: "x-anthropic-billing-hdr"},
}

// ScrubReport 一次脱敏的统计（进审计与日志，不含任何内容）。
type ScrubReport struct {
	// Changed 是否发生了改写（shadow 档下为 false，但 Hits 会有值）。
	Changed bool
	// Messages 被改写（或 shadow 下被判定需要改写）的消息条数。
	Messages int
	// Hits 各规则命中次数，键是规则名。
	Hits map[string]int
}

// Total 命中总次数。
func (r ScrubReport) Total() int {
	n := 0
	for _, v := range r.Hits {
		n += v
	}
	return n
}

// ScrubModeOf 归一化脱敏档位字符串；未知值一律回落到 off（保守）。
func ScrubModeOf(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ScrubOn:
		return ScrubOn
	case ScrubShadow:
		return ScrubShadow
	default:
		return ScrubOff
	}
}

// ScrubRequest 按档位处理上游请求体（openai chat/completions 形态）。
//
//   - off：原样返回（零解析开销）；
//   - shadow：解析并统计命中，但返回原始字节；
//   - on：改写 messages[].content / reasoning_content / tool_calls[].function.arguments。
//
// 解析失败、顶层不是对象、或 messages 不是数组时原样返回：宁可漏洗也不能
// 让请求因为脱敏而发不出去。
func ScrubRequest(mode string, body []byte) ([]byte, ScrubReport, error) {
	report := ScrubReport{Hits: map[string]int{}}
	if ScrubModeOf(mode) == ScrubOff {
		return body, report, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, report, nil
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, report, nil
	}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// 逐字段原地改写，[changed] 汇总本条是否被改过。
		changed := false
		if c, ok := m["content"]; ok {
			if v, ch := scrubContent(c, &report); ch {
				m["content"] = v
				changed = true
			}
		}
		if rc, ok := m["reasoning_content"].(string); ok {
			if v, ch := scrubText(rc, &report); ch {
				m["reasoning_content"] = v
				changed = true
			}
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			if ch := scrubToolCalls(tcs, &report); ch {
				changed = true
			}
		}
		if changed {
			report.Messages++
		}
	}
	if ScrubModeOf(mode) == ScrubShadow {
		// 只统计：丢弃解析结果，返回原始字节（保证影子档零行为差异）
		report.Changed = false
		report.Hits = countOnly(report.Hits)
		return body, report, nil
	}
	if report.Messages == 0 {
		return body, report, nil
	}
	out, err := scrubMarshal(obj)
	if err != nil {
		return body, report, nil
	}
	report.Changed = true
	return out, report, nil
}

// scrubMarshal 序列化脱敏后的对象：关闭 HTML 转义（与 NormalizeRequest 同约定），
// 否则 prompt 里的 <>& 会被写成 \u003c 之类的转义，白白产生字节漂移。
func scrubMarshal(obj map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// countOnly 影子档下的命中计数已按规则累加，这里保持原样返回（可读性占位）。
func countOnly(hits map[string]int) map[string]int { return hits }

// scrubContent 处理 string 或 [{type:text,text:...}] 两种 content 形态。
// 返回改后的值与是否发生改写（shadow 档下 ch 仍为 true，供统计用）。
func scrubContent(v any, report *ScrubReport) (any, bool) {
	switch c := v.(type) {
	case string:
		return scrubText(c, report)
	case []any:
		changed := false
		out := make([]any, 0, len(c))
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok || p["type"] != "text" {
				out = append(out, part)
				continue
			}
			txt, ok := p["text"].(string)
			if !ok {
				out = append(out, part)
				continue
			}
			if nv, ch := scrubText(txt, report); ch {
				cp := map[string]any{}
				for k, v := range p {
					cp[k] = v
				}
				cp["text"] = nv
				out = append(out, cp)
				changed = true
				continue
			}
			out = append(out, part)
		}
		return out, changed
	}
	return v, false
}

// scrubToolCalls 处理 assistant.tool_calls[].function.arguments（也是文本）。
func scrubToolCalls(tcs []any, report *ScrubReport) bool {
	changed := false
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if nv, ch := scrubText(args, report); ch {
			fn["arguments"] = nv
			changed = true
		}
	}
	return changed
}

// scrubText 单段文本脱敏。返回（新值, 是否命中）。
//
// 命中判定与改写分离：先 hasScrubFingerprint 快速跳过（绝大多数消息不含指纹），
// 再按规则顺序改写，最后 TrimSpace（对齐 wb_proxy.py:2427）。
func scrubText(text string, report *ScrubReport) (string, bool) {
	if text == "" || !hasScrubFingerprint(text) {
		return text, false
	}
	out := text
	for _, r := range scrubRules {
		n := 0
		if r.Pattern != nil {
			n = len(r.Pattern.FindAllStringIndex(out, -1))
			if n > 0 {
				out = r.Pattern.ReplaceAllString(out, r.Replace)
			}
		} else if strings.Contains(out, r.Pair[0]) {
			n = strings.Count(out, r.Pair[0])
			out = strings.ReplaceAll(out, r.Pair[0], r.Pair[1])
		}
		if n > 0 {
			report.Hits[r.Name] += n
		}
	}
	// cc_* 片段可能因为前一次删除而相邻暴露（wb2a 用 while 收敛），
	// 规则里的 cc_kv 已是全局替换，这里再兜一次确保收敛。
	if strings.Contains(out, "cc_") {
		if rr := scrubRules[len(scrubRules)-2]; rr.Pattern != nil {
			if n := len(rr.Pattern.FindAllStringIndex(out, -1)); n > 0 {
				out = rr.Pattern.ReplaceAllString(out, "")
				report.Hits[rr.Name] += n
			}
		}
	}
	return strings.TrimSpace(out), true
}

// scrubOmOJunior 连续 OmO 指纹（唯一被 A/B 验证过的连续片段，大小写不敏感）。
// 单独的 "OhMyOpenCode" / "Sisyphus-Junior" 上游是接受的，不作为命中依据。
var scrubOmOJunior = regexp.MustCompile(`(?i)Sisyphus-Junior - Focused executor from OhMyOpenCode`)

// hasScrubFingerprint 快速命中探测（wb_proxy.py:2400-2405）：
// 先查固定特征串，再查裸 header 名与连续 OmO 指纹（后两者用正则）。
func hasScrubFingerprint(text string) bool {
	for _, f := range scrubFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	if strings.Contains(strings.ToLower(text), "x-anthropic-billing-header") {
		return true
	}
	return scrubOmOJunior.MatchString(text)
}
