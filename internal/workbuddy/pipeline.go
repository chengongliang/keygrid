package workbuddy

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
)

// pipeline.go 上游请求体（OpenAI chat/completions 形态）归一化管线。
//
// 忠实移植 wb2a Python 网关 wb_proxy.py 的 build_upstream_body：规则顺序敏感，
// 每条注释标注 WBP 行号。marshal 关闭 HTML 转义，避免把 prompt 里的 <>& 改写
// 成 \u003c（WBP 用 json.dumps(ensure_ascii=False)）。
//
// 与 WBP 的差异：WBP 先做 role/backfill 再拼 body，本函数按 KeyGrid 接线点重排为
// 「先落 body、逐条归一化」，结果等价；并额外接受 model_map 的上游名 upModel。

// NormalizeRequest 归一化上游请求体（OpenAI chat/completions 形态）。
//
// p 用于判定区域（两区域共用同一上游协议，暂不分支）；upModel 是上游模型名
// （需写回 body）；forceStream 为 true 时强制 stream:true 并补
// stream_options.include_usage。
//
// 解析失败或顶层不是 JSON 对象时原样返回入参字节与 nil error：请求仍应可转发，
// 由上游给出更准确的报错，不能因归一化失败丢掉整个请求。
func NormalizeRequest(p *model.Provider, upModel string, body []byte, forceStream bool) ([]byte, error) {
	_ = p
	// 快路径：无强制流式且所有改写触发串都不出现时原样透传，省一次 JSON 往返。
	if !forceStream && normFastPath(body, upModel) {
		return body, nil
	}
	obj := map[string]any{}
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, nil
	}
	// WBP:3000 body["model"] = model —— 别名要覆盖回上游模型名。
	if upModel != "" {
		if cur, _ := obj["model"].(string); cur != upModel {
			obj["model"] = upModel
		}
	}
	upName := upModel
	if upName == "" {
		upName, _ = obj["model"].(string)
	}

	// 1. 丢顶层 _ 前缀私有键（Responses 路径遗留的内部标记）。WBP:2989-2992
	for k := range obj {
		if strings.HasPrefix(k, "_") {
			delete(obj, k)
		}
	}
	// 2. messages[].role 的 developer → system（幂等安全网）。WBP:2348-2363
	//    KeyGrid relay.buildUpstreamBody 已做过一次，这里用字节探测兜底。
	if bytes.Contains(body, []byte("developer")) {
		normDeveloperRoles(obj)
	}
	// 3. 工具配对自愈。WBP:2477（repack）/ WBP:2546（cleanup）
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = toolRepackResults(msgs)
		msgs, _ = toolCleanupOrphans(msgs)
		obj["messages"] = msgs
	}
	// 4. max_completion_tokens → max_tokens（方向由上游决定）。WBP:2756
	normMaxTokens(obj)
	// 5. tool_choice 归一化（对象触发上游 11101）。WBP:2664
	normToolChoice(obj)
	// 6. 顶层 name 形态的 function 工具包成 chat function schema。WBP:2705
	normTools(obj)
	// 7. DeepSeek thinking 注入：thinking.type=enabled 单独不打开 reasoning trace，
	//    必须搭一个 effort；客户端 disabled / effort=none 时不注入。WBP:2964-3029
	thinkingEnabled := normDeepSeekThinking(obj, upName)
	// 8. reasoning_content 回填：上游 11155 要求 thinking 开启时 assistant 都带
	//    reasoning_content，且校验 len(reasoning)>0。WBP:2622
	if msgs, ok := obj["messages"].([]any); ok {
		normBackfillReasoning(msgs, upName, thinkingEnabled)
	}
	// 9. 强制流式 + include_usage；不覆盖调用方已给的 stream_options。WBP:3039-3041
	if forceStream {
		obj["stream"] = true
		if _, ok := obj["stream_options"]; !ok {
			obj["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	return normMarshal(obj)
}

// normFastProbes 触发完整归一化的字节探测串；命中任一即放弃快路径。
// 额外探测 "_ 是规则 1 的保守保险（宁可多走一次解析也不能漏掉顶层私有键）。
var normFastProbes = [][]byte{
	[]byte("developer"),
	[]byte(`"tools"`),
	[]byte("tool_choice"),
	[]byte("max_completion_tokens"),
	[]byte("thinking"),
	[]byte("reasoning"),
	[]byte(`"tool"`), // role:"tool"
	[]byte(`"_`),
}

// normFastPath 判断请求体能否原样透传（forceStream 由调用方先判）。
//
// 模型是否已就位用「body 里是否含带引号的 upModel」保守近似：不含就一定需要
// 改写，故只有「不含即不放行」这一侧要保证可靠。
func normFastPath(body []byte, upModel string) bool {
	if upModel != "" && !bytes.Contains(body, []byte(`"`+upModel+`"`)) {
		return false
	}
	for _, probe := range normFastProbes {
		if bytes.Contains(body, probe) {
			return false
		}
	}
	return true
}

// normMarshal 序列化归一化后的 body（不转义 HTML）。
func normMarshal(obj map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// normDeveloperRoles 就地改 developer role 为 system。WBP:2348-2363。
func normDeveloperRoles(obj map[string]any) {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		if m, ok := raw.(map[string]any); ok && m["role"] == "developer" {
			m["role"] = "system"
		}
	}
}

// toolRepackResults 让每个 assistant tool_calls 批次紧跟其结果块（只重排，
// 结果相对顺序不变，闯入者移到批次后面）。WBP:2477-2544。
func toolRepackResults(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		calls, ok2 := m["tool_calls"].([]any)
		if !ok || m["role"] != "assistant" || !ok2 || len(calls) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tc := range calls {
			if tcm, ok := tc.(map[string]any); ok {
				if tid, ok := tcm["id"].(string); ok && tid != "" {
					want[tid] = true
				}
			}
		}
		out = append(out, m)
		i++
		var results, between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				tid, _ := mm["tool_call_id"].(string)
				if tid == "" || !want[tid] {
					break
				}
				results = append(results, mm)
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break
			}
			// 下一个 assistant.tool_calls 开启新批次，交回外层循环。
			if role == "assistant" {
				if cs, ok := mm["tool_calls"].([]any); ok && len(cs) > 0 {
					break
				}
			}
			between = append(between, mm)
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// toolCleanupOrphans 对称裁剪无结果 tool_calls 与无调用的 tool 结果：
// 两侧按同一份 id 集合裁剪，绝不留半配对 / 空 tool_calls。WBP:2546-2606。
func toolCleanupOrphans(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs, resultIDs := map[string]bool{}, map[string]bool{}
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch m["role"] {
		case "tool":
			if tid, ok := m["tool_call_id"].(string); ok && tid != "" {
				resultIDs[tid] = true
			}
		case "assistant":
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, tc := range calls {
					if tcm, ok := tc.(map[string]any); ok {
						if tid, ok := tcm["id"].(string); ok && tid != "" {
							callIDs[tid] = true
						}
					}
				}
			}
		}
	}
	if len(callIDs) == 0 && len(resultIDs) == 0 {
		return messages, false
	}
	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}
	changed := false
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		calls, ok := m["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		kept := make([]any, 0, len(calls))
		for _, tc := range calls {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			if tid, ok := tcm["id"].(string); ok && keep[tid] {
				kept = append(kept, tc)
			}
		}
		if len(kept) == len(calls) {
			continue
		}
		changed = true
		if len(kept) > 0 {
			m["tool_calls"] = kept
		} else {
			delete(m, "tool_calls")
		}
	}
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if ok && m["role"] == "tool" {
			tid, _ := m["tool_call_id"].(string)
			if tid == "" || !keep[tid] {
				changed = true
				continue
			}
		}
		if ok && len(m) == 0 { // 裁空的消息不留空壳
			continue
		}
		out = append(out, raw)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// normMaxTokens 把 max_completion_tokens 别名转成上游唯一接受的 max_tokens：
// pop 别名；已有 max_tokens 则不覆盖；仅整数且 >0 才落值。WBP:2756-2767。
func normMaxTokens(obj map[string]any) {
	alias, ok := obj["max_completion_tokens"]
	if !ok {
		return
	}
	delete(obj, "max_completion_tokens")
	if _, exists := obj["max_tokens"]; exists {
		return
	}
	if val, ok := normIntLike(alias); ok && val > 0 {
		obj["max_tokens"] = val
	}
}

// normIntLike 近似 Python int(x)：JSON 数字截断取整，字符串走 ParseInt，
// 其余类型不可转换。
func normIntLike(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case string:
		out, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return out, err == nil
	}
	return 0, false
}

// normToolChoice 归一化 tool_choice 为上游只接受的字符串形态。WBP:2664-2702。
//
// 关键取舍：tool_choice=="none"（字符串或对象）只降级字段本身，不删 tools ——
// 删掉能力声明会让模型把调用退化成 DSML/伪 JSON 文本（WBP 注释：Agent 死循环成因）。
func normToolChoice(obj map[string]any) {
	tc, exists := obj["tool_choice"]
	if !exists {
		return
	}
	if s, ok := tc.(string); ok {
		if strings.EqualFold(strings.TrimSpace(s), "none") {
			obj["tool_choice"] = "none"
		}
		return
	}
	d, ok := tc.(map[string]any)
	if !ok {
		delete(obj, "tool_choice")
		return
	}
	switch typ := strings.ToLower(strings.TrimSpace(normString(d["type"]))); typ {
	case "none":
		obj["tool_choice"] = "none"
	case "auto", "required":
		obj["tool_choice"] = typ
	case "function":
		name := ""
		if fn, ok := d["function"].(map[string]any); ok {
			name = normString(fn["name"])
		}
		if name == "" {
			name = normString(d["name"])
		}
		if name = strings.TrimSpace(name); name == "" {
			name = "auto"
		}
		obj["tool_choice"] = name
	default:
		delete(obj, "tool_choice")
	}
}

// normTools 把「顶层 name」形态的 function 工具包成 function schema；非 dict
// 条目丢弃，其余原样透传。WBP:2705-2731。
func normTools(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}
	norm := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if _, hasName := tm["name"]; hasName && tm["type"] == "function" {
			if _, hasFn := tm["function"]; !hasFn {
				fn := map[string]any{
					"name":        normString(tm["name"]),
					"description": normString(tm["description"]),
					"parameters":  tm["parameters"],
				}
				if fn["parameters"] == nil {
					fn["parameters"] = map[string]any{}
				}
				if strict, ok := tm["strict"]; ok {
					fn["strict"] = strict
				}
				norm = append(norm, map[string]any{"type": "function", "function": fn})
				continue
			}
		}
		norm = append(norm, t)
	}
	obj["tools"] = norm
}

// normDeepSeekThinking 注入 DeepSeek thinking/effort 并返回 thinking 是否 enabled
// （供规则 8）。仅 deepseek 系；客户端显式 disabled 或 effort==none 不注入；
// thinking 缺失时补 {"type":"enabled"}；effort 缺失时补 "high"。
//
// 实测（WBP 注释）：enabled + 无 effort → reasoning_tokens=0；effort=high →
// reasoning_tokens=37、reasoning_content 长度 117。WBP:2964-3029。
func normDeepSeekThinking(obj map[string]any, upName string) bool {
	if !strings.HasPrefix(strings.ToLower(upName), "deepseek") {
		return false
	}
	thinkingType := ""
	if th, ok := obj["thinking"].(map[string]any); ok {
		thinkingType = strings.ToLower(strings.TrimSpace(normString(th["type"])))
	}
	effort := normEffort(obj)
	if thinkingType != "disabled" && strings.ToLower(strings.TrimSpace(effort)) != "none" {
		if _, ok := obj["thinking"]; !ok {
			obj["thinking"] = map[string]any{"type": "enabled"}
		}
		if effort == "" {
			// TODO: 后续从模型目录读 defaultEffort（WBP:model_default_effort）。
			obj["reasoning_effort"] = "high"
		}
	}
	th, _ := obj["thinking"].(map[string]any)
	return strings.ToLower(strings.TrimSpace(normString(th["type"]))) == "enabled"
}

// normEffort 取客户端 effort（对齐 WBP 的 `reasoning_effort or reasoningEffort`：
// 前一个为空才看后一个）。
func normEffort(obj map[string]any) string {
	if s, ok := obj["reasoning_effort"].(string); ok && s != "" {
		return s
	}
	if s, ok := obj["reasoningEffort"].(string); ok {
		return s
	}
	return ""
}

// normBackfillReasoning 回填 reasoning_content 并把 reasoning 镜像成非空值：
// 门槛为 thinkingEnabled 或 hasTrace（历史已有痕迹）；非字符串 reasoning_content
// 算缺失；空占位写 " "（上游校验 len>0）。仅 deepseek 系。WBP:2622-2657。
func normBackfillReasoning(messages []any, upName string, thinkingEnabled bool) {
	if !strings.HasPrefix(strings.ToLower(upName), "deepseek") {
		return
	}
	hasTrace := false
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["reasoning"].(string); ok && s != "" {
			hasTrace = true
			break
		}
		if _, ok := m["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return
	}
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		rc, ok := m["reasoning_content"].(string)
		if !ok {
			if legacy, ok := m["reasoning"].(string); ok {
				rc = legacy
			} else {
				rc = ""
			}
			m["reasoning_content"] = rc
		}
		if existing, ok := m["reasoning"].(string); !ok || existing == "" {
			if rc != "" {
				m["reasoning"] = rc
			} else {
				m["reasoning"] = " "
			}
		}
	}
}

// normString 取字符串值；非字符串返回空串。
func normString(v any) string {
	s, _ := v.(string)
	return s
}
