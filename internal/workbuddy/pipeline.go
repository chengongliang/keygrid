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
// 有意不移植 WBP:10746-10768 的 apply_prompt_mode，也不移植 WBP:10794-10795
// （messages[0] 非 system 时强制插入网关 SYSTEM_PROMPT）：KeyGrid 不注入任何
// 网关系统提示，见 docs/workbuddy-integration-design.md §3.7（第 389 行）。

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
	// 3. 工具配对自愈：先合并被客户端拆开的相邻 tool_calls 声明、再重排结果块、
	//    最后对称裁剪孤儿。顺序有语义：merge 必须早于 repack，否则 repack 看到的是
	//    半截批次。WBP:2477 / WBP:2546 / WBP:10071（merge 为本轮新增）
	if msgs, ok := obj["messages"].([]any); ok {
		normToolArguments(msgs)
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = toolRepackResults(msgs)
		msgs, _ = toolCleanupOrphans(msgs)
		obj["messages"] = msgs
	}
	normMaxTokens(obj)  // 4. max_completion_tokens → max_tokens。WBP:2756
	normToolChoice(obj) // 5. tool_choice 归一化（对象触发上游 11101）。WBP:2664
	normTools(obj)      // 6. 顶层 name 工具包成 function schema。WBP:2705
	normStop(obj)       // 7. stop 字符串包成数组（上游 []string 收到字符串 400/11101）。WBP:10419
	// 8. 未声明 max_tokens 时填模型目录的默认最大输出 token；未知模型保持缺省。WBP:10880
	if _, ok := obj["max_tokens"]; !ok {
		if def := modelDefaultMaxOutputTokens(upName); def > 0 {
			obj["max_tokens"] = def
		}
	}
	// 9. DeepSeek thinking 注入：仅 enabled 不足以打开 reasoning trace，必须搭一个
	//    effort；客户端 disabled / effort=none 不注入。WBP:2964-3029
	thinkingEnabled := normDeepSeekThinking(obj, upName)
	// 10. reasoning_content 回填（上游 11155 要求 assistant 都带，且 len(reasoning)>0）。WBP:2622
	if msgs, ok := obj["messages"].([]any); ok {
		normBackfillReasoning(msgs, upName, thinkingEnabled)
	}
	// 11. 强制流式；不覆盖调用方已给的 stream_options。WBP:3039-3041
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
	[]byte(`"stop"`), // stop 需要用带引号的键探测：普通 prompt 文本里的 stop 不该触发
	[]byte(`"functions"`),
	[]byte(`"_`), // 额外的 tool_calls/deepseek/"_ 是保守保险：漏掉它们会让规则
	// 3 / 7 / 1 在无 upModel 时被快路径跳过；宁可多走一次解析也不能漏改写。
}

// normFastPath 判断请求体能否原样透传（forceStream 由调用方先判）。模型是否已
// 就位用「body 含带引号的 upModel」保守近似：不含即不放行，这一侧必须可靠。
func normFastPath(body []byte, upModel string) bool {
	if upModel != "" && !bytes.Contains(body, []byte(`"`+upModel+`"`)) {
		return false
	}
	// 默认 max_tokens 填充（规则 8）：body 未携带 max_tokens 时是否要填取决于模型。
	// 用带引号的键探测，prompt 文本里出现 max_tokens 不会误命中（JSON 里引号会被
	// 转义成 \"，故不会匹配）。已知模型必须走完整路径；upModel 为空时模型名要先
	// 解析才知道，同样保守走完整路径。
	if !bytes.Contains(body, []byte(`"max_tokens"`)) {
		if upModel == "" || modelDefaultMaxOutputTokens(upModel) > 0 {
			return false
		}
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

// normEmptyContent 判断「没有内容」：null / "" / [] 都算无内容（客户端对空内容的
// 形状没有共识，漏判 [] 正是最早的 11148 案例）。其它类型（数字/对象/布尔）不算空。
// WBP:10043-10057。
func normEmptyContent(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}

// mergeReasoningContent 把 src 的 reasoning_content 折进 dst：两边都有时用换行
// 拼接。非字符串 / 空串直接忽略。WBP:10059-10069。
func mergeReasoningContent(dst, src map[string]any) {
	rc, _ := src["reasoning_content"].(string)
	if rc == "" {
		return
	}
	if prev, _ := dst["reasoning_content"].(string); prev != "" {
		dst["reasoning_content"] = prev + "\n" + rc
		return
	}
	dst["reasoning_content"] = rc
}

// mergeAdjacentToolCalls 合并「背靠背」的两个 assistant 消息（同一并行 tool_calls
// 批次被客户端拆成多条相邻 assistant）。DeepSeek 系上游对该形状回 400 /
// tool_call_sequence_broken，且因为每次重试都重放同一段历史，换账号也救不回来。
// 条件从严，不发明语义：
//  1. 两条消息必须相邻；
//  2. 后一条的 content 必须为空（null/""/[]）；
//  3. 前一条必须已经是带 tool_calls 的 assistant。
//
// 另一形态：后一条是纯字符串 content 的 assistant，而前一条带 tool_calls 但自身
// 无内容 —— 同一拆分批次的另一种重放顺序，折叠进前一条。
// 两种形态都保留 reasoning_content（DeepSeek 多轮 thinking 要求回传）。
// WBP:10071-10126。
func mergeAdjacentToolCalls(messages []any) ([]any, bool) {
	if len(messages) < 2 {
		return messages, false
	}
	out, changed := make([]any, 0, len(messages)), false
	for _, raw := range messages {
		m := normMap(raw)
		if m != nil && m["role"] == "assistant" && len(out) > 0 {
			if prev := normMap(out[len(out)-1]); prev != nil && prev["role"] == "assistant" {
				calls := normSlice(m["tool_calls"])
				prevCalls := normSlice(prev["tool_calls"])
				// 形态一：本条声明 tool_calls 且自身无内容。
				if len(calls) > 0 && normEmptyContent(m["content"]) && len(prevCalls) > 0 {
					prev["tool_calls"] = append(prevCalls, calls...)
					mergeReasoningContent(prev, m)
					changed = true
					continue
				}
				// 形态二：本条是纯字符串内容；前一条带 tool_calls 且自身无内容。
				if _, hasCalls := m["tool_calls"]; !hasCalls && len(prevCalls) > 0 {
					if content, ok := m["content"].(string); ok && content != "" &&
						normEmptyContent(prev["content"]) {
						prev["content"] = content
						mergeReasoningContent(prev, m)
						changed = true
						continue
					}
				}
			}
		}
		out = append(out, raw)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

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
			if role == "assistant" {
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

// normStop 归一化 stop 为上游唯一接受的字符串数组形态。WBP:10419-10448。
// OpenAI 把 stop 声明成 string | array，客户端传单字符串完全合法；上游 Go 端却把
// 该字段定死为 []string，收到字符串直接 400 / 11101。字符串包成单元素数组语义等价；
// 数组只保留其中非空字符串且顺序不变；空串 / 空数组 / null / 其它类型一律删字段
// ——它们的实际含义都是「没有停止序列」，而空值本身在上游同样判参数不合法。
func normStop(obj map[string]any) {
	raw, exists := obj["stop"]
	if !exists {
		return
	}
	var out []any
	switch t := raw.(type) {
	case string:
		if t != "" {
			out = []any{t}
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) > 0 {
		obj["stop"] = out
		return
	}
	delete(obj, "stop")
}

// normTools 兼容旧版 functions 与扁平工具声明，并修复上游拒绝的参数 Schema。
func normTools(obj map[string]any) {
	if functions, exists := obj["functions"]; exists {
		if _, hasTools := obj["tools"]; !hasTools {
			var tools []any
			for _, raw := range normSlice(functions) {
				if function := normMap(raw); function != nil {
					tools = append(tools, map[string]any{"type": "function", "function": function})
				}
			}
			obj["tools"] = tools
			delete(obj, "functions")
		}
	}
	tools := normSlice(obj["tools"])
	if len(tools) == 0 {
		delete(obj, "tools")
		return
	}
	norm := make([]any, 0, len(tools))
	for _, t := range tools {
		tm := normMap(t)
		if tm == nil {
			continue
		}
		if _, hasName := tm["name"]; hasName {
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
				normToolParameters(fn)
				norm = append(norm, map[string]any{"type": "function", "function": fn})
				continue
			}
		}
		if function := normMap(tm["function"]); function != nil {
			tm["type"] = "function"
			normToolParameters(function)
		}
		norm = append(norm, t)
	}
	if len(norm) > 0 {
		obj["tools"] = norm
	} else {
		delete(obj, "tools")
	}
}

func normToolArguments(messages []any) {
	for _, raw := range messages {
		message := normMap(raw)
		if message["role"] != "assistant" {
			continue
		}
		for _, rawCall := range normSlice(message["tool_calls"]) {
			function := normMap(normMap(rawCall)["function"])
			switch arguments := function["arguments"].(type) {
			case map[string]any, []any:
				var encoded bytes.Buffer
				encoder := json.NewEncoder(&encoded)
				encoder.SetEscapeHTML(false)
				if encoder.Encode(arguments) == nil {
					function["arguments"] = strings.TrimSuffix(encoded.String(), "\n")
				}
			}
		}
	}
}

func normToolParameters(function map[string]any) {
	parameters := normMap(function["parameters"])
	if parameters == nil {
		parameters = map[string]any{}
	}
	parameters["type"] = "object"
	if normMap(parameters["properties"]) == nil {
		parameters["properties"] = map[string]any{}
	}
	normSchema(parameters)
	function["parameters"] = parameters
}

func normSchema(schema map[string]any) {
	for _, key := range []string{"description", "title", "format", "pattern", "items"} {
		if schema[key] == nil {
			delete(schema, key)
		}
	}
	if typeName, ok := schema["type"].(string); ok {
		typeName = normSchemaType(typeName)
		switch typeName {
		case "string", "number", "integer", "boolean", "array", "object", "null":
			schema["type"] = typeName
		default:
			delete(schema, "type")
			if _, exists := schema["properties"]; exists {
				schema["type"] = "object"
			} else if _, exists := schema["items"]; exists {
				schema["type"] = "array"
			}
		}
	} else if types, ok := schema["type"].([]any); ok {
		for index, raw := range types {
			if typeName, ok := raw.(string); ok {
				types[index] = normSchemaType(typeName)
			}
		}
	}
	if _, exists := schema["properties"]; exists && schema["type"] == nil {
		schema["type"] = "object"
	}
	if schema["type"] == "object" && normMap(schema["properties"]) == nil {
		schema["properties"] = map[string]any{}
	}
	if required, exists := schema["required"]; exists {
		var cleaned []any
		if name, ok := required.(string); ok {
			required = []any{strings.TrimSpace(name)}
		}
		for _, raw := range normSlice(required) {
			if name, ok := raw.(string); ok && strings.TrimSpace(name) != "" {
				cleaned = append(cleaned, name)
			}
		}
		if len(cleaned) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = cleaned
		}
	}
	if items, ok := schema["items"].([]any); ok {
		first := map[string]any{"type": "string"}
		for _, raw := range items {
			if item := normMap(raw); item != nil {
				first = item
				break
			}
		}
		schema["items"] = first
	}
	if items := normMap(schema["items"]); items != nil {
		normSchema(items)
	}
	for _, key := range []string{"properties", "$defs", "definitions"} {
		for _, raw := range normMap(schema[key]) {
			if nested := normMap(raw); nested != nil {
				normSchema(nested)
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		for _, raw := range normSlice(schema[key]) {
			if nested := normMap(raw); nested != nil {
				normSchema(nested)
			}
		}
	}
}

func normSchemaType(typeName string) string {
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	switch typeName {
	case "str", "text":
		return "string"
	case "int":
		return "integer"
	case "bool":
		return "boolean"
	case "dict":
		return "object"
	case "list":
		return "array"
	case "float":
		return "number"
	default:
		return typeName
	}
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

// wbModelMaxOutputTokens 模型目录声明的默认最大输出 token 数，键为小写模型 id。
//
// 上游 wb_proxy.py 的 model_default_max_output_tokens（WBP:10880-10891）走
// model_catalog_meta：先判区域，再查运行时模型缓存，缓存冷时回落到 wb_catalog 的
// STATIC_CN_MODELS / STATIC_INTL_MODELS 静态表，最后按 id（忽略大小写）精确匹配
// meta["maxOutputTokens"]。我们的 catalog.go 只落模型 id、不落目录元数据，因此这里
// 只移植该查表形状的静态部分：键取自本仓库内嵌快照 snapshot/models_{intl,cn}.json
// 的并集（即我们已经知道的那批模型 id），值取参考静态表同 id 的 maxOutputTokens。
// 两区域取值无冲突，故合并成一张表、不再分区域。
//
// 未收录的 id（含未在我们快照里的 default-model / fast-model / balanced-model /
// primary-model / deep-model 五个档位别名）返回 0，调用方保持 max_tokens 缺省 ——
// 与参考「未知模型返回 None」的行为一致。后续 kimi 等目录补齐元数据后，这张表可
// 直接换成运行时目录查询。
var wbModelMaxOutputTokens = map[string]int64{
	"deepseek-r1-0528":       8192,
	"deepseek-r1-0528-lkeap": 16000,
	"deepseek-v3-0324":       8192,
	"deepseek-v3-0324-lkeap": 16000,
	"deepseek-v3-1":          32000,
	"deepseek-v3-1-lkeap":    32000,
	"deepseek-v3-1-volc":     32000,
	"deepseek-v3-2-volc":     32000,
	"deepseek-v4-flash":      50000,
	"deepseek-v4-pro":        50000,
	"deepseek-v4.1-flash":    128000,
	"default-1.1":            8192,
	"default-1.2":            24000,
	"gemini-3.5-flash":       65536,
	"glm-4.6":                32000,
	"glm-4.6v":               32000,
	"glm-5.0-turbo":          48000,
	"glm-5.1":                48000,
	"glm-5.2":                48000,
	"glm-5.3":                48000,
	"glm-5.3-flash":          32000,
	"glm-5v-turbo":           64000,
	"gpt-5.3-codex":          72000,
	"gpt-5.4":                72000,
	"gpt-5.5":                128000,
	"gpt-5.6-luna":           128000,
	"gpt-5.6-sol":            128000,
	"gpt-5.6-terra":          128000,
	"gpt-6-astra":            128000,
	"grok-4.7":               128000,
	"hunyuan-2.0-instruct":   16000,
	"hunyuan-chat":           8192,
	"hy3":                    64000,
	"hy3-x":                  64000,
	"hy4-preview":            64000,
	"hy4-preview-dev":        64000,
	"hy4-preview-f":          64000,
	"hy4-preview-x":          64000,
	"kimi-k2-instruct-taiji": 8192,
	"kimi-k2-thinking":       32000,
	"kimi-k2.5":              32000,
	"kimi-k2.6":              32000,
	"kimi-k2.7":              32000,
	"kimi-k2.8-preview":      32000,
	"kimi-k3":                32000,
	"kimi-k3-1":              32000,
	"minimax-m2.5":           48000,
	"minimax-m2.7":           48000,
	"minimax-m3":             128000,
}

// modelDefaultMaxOutputTokens 返回模型目录声明的默认最大输出 token 数；未知模型
// （或声明值 <=0）返回 0，调用方不填字段。查表忽略大小写与首尾空白。WBP:10880。
func modelDefaultMaxOutputTokens(model string) int64 {
	if model == "" {
		return 0
	}
	return wbModelMaxOutputTokens[strings.ToLower(strings.TrimSpace(model))]
}
