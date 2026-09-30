package workbuddy

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
)

// pipeline.go 上游请求体（OpenAI chat/completions 形态）归一化管线，忠实移植
// wb2a 网关 wb_proxy.py 的 build_upstream_body（下称 WBP，注释标注行号）。
// marshal 关闭 HTML 转义，避免把 prompt 里的 <>& 改写成 \u003c。
// 与 WBP 的差异：本函数按 KeyGrid 接线点重排为「先落 body、逐条归一化」，
// 结果等价，并额外接受 model_map 的上游名 upModel。

// NormalizeRequest 归一化上游请求体（OpenAI chat/completions 形态）。
// p 用于判定区域（两区域共用同一上游协议，暂不分支）；upModel 是上游模型名
// （需写回 body）；forceStream 为 true 时强制 stream:true 并补
// stream_options.include_usage。
// 解析失败或顶层不是 JSON 对象时原样返回入参字节与 nil error：请求仍应可转发。
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
	if upModel != "" { // WBP:3000 body["model"] = model
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
	// 2. developer → system；relay.buildUpstreamBody 已做过一次，此处字节探测兜底。WBP:2348-2363
	if bytes.Contains(body, []byte("developer")) {
		for _, raw := range normSlice(obj["messages"]) {
			if m := normMap(raw); m["role"] == "developer" {
				m["role"] = "system"
			}
		}
	}
	// 3. 工具配对自愈：先重排结果块、再对称裁剪孤儿。WBP:2477 / WBP:2546
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = toolRepackResults(msgs)
		msgs, _ = toolCleanupOrphans(msgs)
		obj["messages"] = msgs
	}
	normMaxTokens(obj)  // 4. max_completion_tokens → max_tokens。WBP:2756
	normToolChoice(obj) // 5. tool_choice 归一化（对象触发上游 11101）。WBP:2664
	normTools(obj)      // 6. 顶层 name 工具包成 function schema。WBP:2705
	// 7. DeepSeek thinking 注入：仅 enabled 不足以打开 reasoning trace，必须搭一个
	//    effort；客户端 disabled / effort=none 不注入。WBP:2964-3029
	thinkingEnabled := normDeepSeekThinking(obj, upName)
	// 8. reasoning_content 回填（上游 11155 要求 assistant 都带，且 len(reasoning)>0）。WBP:2622
	if msgs, ok := obj["messages"].([]any); ok {
		normBackfillReasoning(msgs, upName, thinkingEnabled)
	}
	// 9. 强制流式；不覆盖调用方已给的 stream_options。WBP:3039-3041
	if forceStream {
		obj["stream"] = true
		if _, ok := obj["stream_options"]; !ok {
			obj["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return body, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// normFastProbes 触发完整归一化的字节探测串；命中任一即放弃快路径。
var normFastProbes = [][]byte{
	[]byte("developer"), []byte(`"tools"`), []byte("tool_choice"),
	[]byte("max_completion_tokens"), []byte("thinking"), []byte("reasoning"),
	[]byte(`"tool"`), []byte("tool_calls"), []byte("deepseek"),
	[]byte(`"_`), // 额外的 tool_calls/deepseek/"_ 是保守保险：漏掉它们会让规则
	// 3 / 7 / 1 在无 upModel 时被快路径跳过；宁可多走一次解析也不能漏改写。
}

// normFastPath 判断请求体能否原样透传（forceStream 由调用方先判）。模型是否已
// 就位用「body 含带引号的 upModel」保守近似：不含即不放行，这一侧必须可靠。
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

// normMap 取 JSON 对象；非对象返回 nil（对 nil map 的读取安全）。
func normMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// normSlice 取 JSON 数组；非数组返回 nil。
func normSlice(v any) []any { s, _ := v.([]any); return s }

// normString 取字符串值；非字符串返回空串。
func normString(v any) string { s, _ := v.(string); return s }

// toolRepackResults 让每个 assistant tool_calls 批次紧跟其结果块（只重排，
// 结果相对顺序不变，闯入者移到批次后面）。WBP:2477-2544。
func toolRepackResults(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out, changed, i := make([]any, 0, len(messages)), false, 0
	for i < len(messages) {
		m := normMap(messages[i])
		calls := normSlice(m["tool_calls"])
		if m["role"] != "assistant" || len(calls) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tc := range calls {
			if tid := normString(normMap(tc)["id"]); tid != "" {
				want[tid] = true
			}
		}
		out = append(out, m)
		i++
		var results, between []any
		for i < len(messages) {
			mm := normMap(messages[i])
			if mm == nil {
				break
			}
			role := normString(mm["role"])
			if role == "tool" {
				if tid := normString(mm["tool_call_id"]); tid == "" || !want[tid] {
					break
				}
				results = append(results, mm)
				if len(between) > 0 {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // 闯入者只在批次结果之后才被搬走
			}
			// 下一个 assistant.tool_calls 开启新批次，交回外层循环。
			if role == "assistant" && len(normSlice(mm["tool_calls"])) > 0 {
				break
			}
			between = append(between, mm)
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

// toolCleanupOrphans 对称裁剪无结果 tool_calls 与无调用的 tool 结果，绝不留
// 半配对 / 空 tool_calls / 空壳消息。WBP:2546-2606。
func toolCleanupOrphans(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs, resultIDs := map[string]bool{}, map[string]bool{}
	for _, raw := range messages {
		m := normMap(raw)
		switch normString(m["role"]) {
		case "tool":
			if tid := normString(m["tool_call_id"]); tid != "" {
				resultIDs[tid] = true
			}
		case "assistant":
			for _, tc := range normSlice(m["tool_calls"]) {
				if tid := normString(normMap(tc)["id"]); tid != "" {
					callIDs[tid] = true
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
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		m := normMap(raw)
		switch normString(m["role"]) {
		case "tool":
			if tid := normString(m["tool_call_id"]); tid == "" || !keep[tid] {
				changed = true
				continue
			}
		case "assistant":
			if calls := normSlice(m["tool_calls"]); len(calls) > 0 {
				kept := make([]any, 0, len(calls))
				for _, tc := range calls {
					if tid := normString(normMap(tc)["id"]); tid != "" && keep[tid] {
						kept = append(kept, tc)
					}
				}
				if len(kept) != len(calls) {
					changed = true
					if len(kept) > 0 {
						m["tool_calls"] = kept
					} else {
						delete(m, "tool_calls")
					}
				}
			}
		}
		if m != nil && len(m) == 0 {
			continue // 裁空的消息不留空壳
		}
		out = append(out, raw)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// normMaxTokens 把 max_completion_tokens 别名转成上游唯一接受的 max_tokens：
// pop 别名；已有 max_tokens 则不覆盖；仅整数且 >0 才落值（近似 Python int(x)）。
// WBP:2756-2767。
func normMaxTokens(obj map[string]any) {
	alias, ok := obj["max_completion_tokens"]
	if !ok {
		return
	}
	delete(obj, "max_completion_tokens")
	if _, exists := obj["max_tokens"]; exists {
		return
	}
	if val, ok := alias.(float64); ok && val > 0 {
		obj["max_tokens"] = int64(val)
		return
	}
	if s, ok := alias.(string); ok {
		if val, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && val > 0 {
			obj["max_tokens"] = val
		}
	}
}

// normToolChoice 归一化 tool_choice 为上游只接受的字符串形态。WBP:2664-2702。
// 关键取舍：tool_choice=="none" 只降级字段本身、不删 tools —— 删掉能力声明会让
// 模型把调用退化成 DSML/伪 JSON 文本（WBP 注释：Agent 死循环成因）。
func normToolChoice(obj map[string]any) {
	tc, exists := obj["tool_choice"]
	if !exists {
		return
	}
	if s, ok := tc.(string); ok {
		if strings.EqualFold(strings.TrimSpace(s), "none") {
			obj["tool_choice"] = "none"
		}
		return // 其它字符串（auto/required/函数名）原样透传
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
		fnName := ""
		if fn := normMap(d["function"]); fn != nil {
			fnName = normString(fn["name"])
		}
		if fnName == "" {
			fnName = normString(d["name"])
		}
		if fnName = strings.TrimSpace(fnName); fnName == "" {
			fnName = "auto"
		}
		obj["tool_choice"] = fnName
	default:
		delete(obj, "tool_choice")
	}
}

// normTools 把「顶层 name」形态的 function 工具包成 function schema；非 dict
// 条目丢弃，其余原样透传。WBP:2705-2731。
func normTools(obj map[string]any) {
	tools := normSlice(obj["tools"])
	if len(tools) == 0 {
		return
	}
	norm := make([]any, 0, len(tools))
	for _, t := range tools {
		tm := normMap(t)
		if tm == nil {
			continue
		}
		if _, hasName := tm["name"]; hasName && tm["type"] == "function" {
			if _, hasFn := tm["function"]; !hasFn {
				params := tm["parameters"]
				if params == nil {
					params = map[string]any{}
				}
				fn := map[string]any{
					"name": normString(tm["name"]), "description": normString(tm["description"]),
					"parameters": params,
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

// normDeepSeekThinking 注入 DeepSeek thinking/effort 并返回 thinking 是否 enabled。
// 仅 deepseek 系；客户端显式 disabled 或 effort==none 不注入；thinking 缺失时补
// {"type":"enabled"}；effort 缺失时补 "high"（WBP 实测 enabled 单独不产生 trace）。
// WBP:2964-3029。
func normDeepSeekThinking(obj map[string]any, upName string) bool {
	if !strings.HasPrefix(strings.ToLower(upName), "deepseek") {
		return false
	}
	thinkingType := ""
	if th := normMap(obj["thinking"]); th != nil {
		thinkingType = strings.ToLower(strings.TrimSpace(normString(th["type"])))
	}
	effort := normString(obj["reasoning_effort"])
	if effort == "" { // 对齐 WBP 的 `reasoning_effort or reasoningEffort`
		effort = normString(obj["reasoningEffort"])
	}
	if thinkingType != "disabled" && strings.ToLower(strings.TrimSpace(effort)) != "none" {
		if _, ok := obj["thinking"]; !ok {
			obj["thinking"] = map[string]any{"type": "enabled"}
		}
		if effort == "" {
			// TODO: 后续从模型目录读 defaultEffort（WBP:model_default_effort）。
			obj["reasoning_effort"] = "high"
		}
	}
	return strings.ToLower(strings.TrimSpace(normString(normMap(obj["thinking"])["type"]))) == "enabled"
}

// normBackfillReasoning 回填 reasoning_content 并把 reasoning 镜像成非空值：门槛
// 为 thinkingEnabled 或 hasTrace；非字符串 reasoning_content 算缺失；空占位写 " "。
// 仅 deepseek 系。WBP:2622-2657。
func normBackfillReasoning(messages []any, upName string, thinkingEnabled bool) {
	if !strings.HasPrefix(strings.ToLower(upName), "deepseek") {
		return
	}
	hasTrace := false
	for _, raw := range messages {
		m := normMap(raw)
		if normString(m["reasoning"]) != "" {
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
		m := normMap(raw)
		if m["role"] != "assistant" {
			continue
		}
		rc, ok := m["reasoning_content"].(string)
		if !ok { // 非字符串（null/数字/缺失）一律算缺失
			rc = normString(m["reasoning"])
			m["reasoning_content"] = rc
		}
		if existing := normString(m["reasoning"]); existing == "" {
			if rc != "" {
				m["reasoning"] = rc
			} else {
				m["reasoning"] = " " // 上游校验长度 > 0
			}
		}
	}
}
