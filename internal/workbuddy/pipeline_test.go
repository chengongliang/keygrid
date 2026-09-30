package workbuddy

import (
	"encoding/json"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

// pipeline_test.go NormalizeRequest 的表驱动单测：无网络、无 DB，全部用
// model.Provider{} 作为渠道参数。

// normRun 调用 NormalizeRequest 并把结果解析成对象。
func normRun(t *testing.T, body, upModel string, forceStream bool) map[string]any {
	t.Helper()
	out, err := NormalizeRequest(&model.Provider{}, upModel, []byte(body), forceStream)
	if err != nil {
		t.Fatalf("NormalizeRequest 返回错误: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("结果不是 JSON 对象: %v (%s)", err, out)
	}
	return m
}

// normTestMessages 取 messages 数组。
func normTestMessages(t *testing.T, m map[string]any) []any {
	t.Helper()
	msgs, ok := m["messages"].([]any)
	if !ok {
		t.Fatalf("messages 缺失或不是数组: %#v", m["messages"])
	}
	return msgs
}

// TestNormalizeFastPathIdentity 验证无改写因素时返回同一底层切片（快路径）。
func TestNormalizeFastPathIdentity(t *testing.T) {
	body := []byte(`{"model":"workbuddy-x","messages":[{"role":"user","content":"hi"}]}`)
	got, err := NormalizeRequest(&model.Provider{}, "workbuddy-x", body, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// 无需 unsafe：返回的切片若与入参共享底层数组，则首元素地址相同。
	if len(got) == 0 || &got[0] != &body[0] {
		t.Fatalf("期望快路径返回同一底层切片, got %s", got)
	}
	// 反向：命中探测串（tools）时必须走完整解析（返回新切片）。
	withTools := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	got2, err := NormalizeRequest(&model.Provider{}, "", withTools, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if &got2[0] == &withTools[0] {
		t.Fatal("命中 tools 探测串时不应走快路径")
	}
}

// TestNormalizeDeveloperRole developer → system，含快路径下 no-op 的场景。
func TestNormalizeDeveloperRole(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "developer 改为 system",
			body: `{"model":"m","messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"}]}`,
			want: []string{"system", "user"},
		},
		{
			name: "已是 system 不动",
			body: `{"model":"m","messages":[{"role":"system","content":"a"}]}`,
			want: []string{"system"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := normRun(t, c.body, "", false)
			msgs := normTestMessages(t, m)
			if len(msgs) != len(c.want) {
				t.Fatalf("消息数 = %d, want %d", len(msgs), len(c.want))
			}
			for i, want := range c.want {
				if got := normString(normMap(msgs[i])["role"]); got != want {
					t.Fatalf("messages[%d].role = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// TestNormalizeToolChoice 覆盖 tool_choice 六个分支及 tools 存活。
func TestNormalizeToolChoice(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any // nil 表示键应被删除
	}{
		{"字符串 none", `"none"`, "none"},
		{"字符串 auto 透传", `"auto"`, "auto"},
		{"对象 none", `{"type":"none"}`, "none"},
		{"对象 auto", `{"type":"auto"}`, "auto"},
		{"对象 required", `{"type":"required"}`, "required"},
		{"对象 function", `{"type":"function","function":{"name":"foo"}}`, "foo"},
		{"对象 function 用顶层 name 兜底", `{"type":"function","name":"bar"}`, "bar"},
		{"对象 function 无名字退 auto", `{"type":"function"}`, "auto"},
		{"对象未知 type 删除", `{"type":"weird"}`, nil},
		{"数字类型删除", `5`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"foo"}}],"tool_choice":` + c.in + `}`
			m := normRun(t, body, "", false)
			got, exists := m["tool_choice"]
			if c.want == nil {
				if exists {
					t.Fatalf("tool_choice 应被删除, got %#v", got)
				}
			} else if !exists || got != c.want {
				t.Fatalf("tool_choice = %#v (exists=%v), want %#v", got, exists, c.want)
			}
			// tool_choice="none" 也不得删掉 tools（删了会让模型退化成伪 JSON）。
			if tools := normSlice(m["tools"]); len(tools) != 1 {
				t.Fatalf("tools 应保留 1 个, got %d", len(tools))
			}
		})
	}
}

// TestNormalizeToolsWrapping 顶层 name 形态包成 function schema，非 dict 丢弃。
func TestNormalizeToolsWrapping(t *testing.T) {
	body := `{"model":"m","messages":[],"tools":[` +
		`{"type":"function","name":"get_x","description":"d","parameters":{"type":"object"},"strict":true},` +
		`"garbage",` +
		`{"type":"function","function":{"name":"keep"}}` +
		`]}`
	m := normRun(t, body, "", false)
	tools := normSlice(m["tools"])
	if len(tools) != 2 {
		t.Fatalf("tools 长度 = %d, want 2（非 dict 条目应被丢弃）", len(tools))
	}
	first := normMap(tools[0])
	fn := normMap(first["function"])
	if normString(first["type"]) != "function" || normString(fn["name"]) != "get_x" ||
		normString(fn["description"]) != "d" || fn["parameters"] == nil || fn["strict"] != true {
		t.Fatalf("包裹结果不正确: %s", tools[0].(map[string]any))
	}
	if _, ok := first["name"]; ok {
		t.Fatal("包裹后不应保留顶层 name")
	}
	if got := normString(normMap(normMap(tools[1])["function"])["name"]); got != "keep" {
		t.Fatalf("已是 function schema 的条目应原样透传, got %q", got)
	}
}

// TestNormalizeMaxCompletionTokens 别名两个方向。
func TestNormalizeMaxCompletionTokens(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantVal   float64
		wantExist bool
	}{
		{"仅别名", `{"model":"m","messages":[],"max_completion_tokens":123}`, 123, true},
		{"别名 + 已有 max_tokens", `{"model":"m","messages":[],"max_tokens":50,"max_completion_tokens":123}`, 50, true},
		{"别名 0 不落值", `{"model":"m","messages":[],"max_completion_tokens":0}`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := normRun(t, c.body, "", false)
			if _, ok := m["max_completion_tokens"]; ok {
				t.Fatal("max_completion_tokens 别名应被 pop")
			}
			got, exists := m["max_tokens"]
			if exists != c.wantExist {
				t.Fatalf("max_tokens exists = %v, want %v", exists, c.wantExist)
			}
			if exists && got != c.wantVal {
				t.Fatalf("max_tokens = %#v, want %v", got, c.wantVal)
			}
		})
	}
}

// TestNormalizeToolPairing 覆盖缺结果、孤儿结果、批次被隔断三种情况，并断言
// 修复后 calls 与 results 对称。
func TestNormalizeToolPairing(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantRoles  []string // 期望的 role 顺序
		wantCalls  int      // 期望残留的 tool_calls 总数
		wantResult int      // 期望残留的 tool 结果数
	}{
		{
			name: "调用缺结果：删除该 tool_calls",
			body: `{"model":"m","messages":[` +
				`{"role":"user","content":"u"},` +
				`{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f"}}]}` +
				`]}`,
			wantRoles: []string{"user", "assistant"}, wantCalls: 0, wantResult: 0,
		},
		{
			name: "孤儿结果：删除 tool 消息",
			body: `{"model":"m","messages":[` +
				`{"role":"user","content":"u"},` +
				`{"role":"tool","tool_call_id":"x","content":"r"}` +
				`]}`,
			wantRoles: []string{"user"}, wantCalls: 0, wantResult: 0,
		},
		{
			name: "消息隔断批次：结果块移回批次后",
			body: `{"model":"m","messages":[` +
				`{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f"}},{"id":"b","type":"function","function":{"name":"g"}}]},` +
				`{"role":"tool","tool_call_id":"a","content":"ra"},` +
				`{"role":"user","content":"note"},` +
				`{"role":"tool","tool_call_id":"b","content":"rb"}` +
				`]}`,
			wantRoles: []string{"assistant", "tool", "tool", "user"}, wantCalls: 2, wantResult: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := normRun(t, c.body, "", false)
			msgs := normTestMessages(t, m)
			if len(msgs) != len(c.wantRoles) {
				t.Fatalf("消息数 = %d, want %d (%v)", len(msgs), len(c.wantRoles), msgs)
			}
			calls, results := 0, 0
			callIDs, resultIDs := map[string]bool{}, map[string]bool{}
			for i, raw := range msgs {
				mm := normMap(raw)
				if got := normString(mm["role"]); got != c.wantRoles[i] {
					t.Fatalf("messages[%d].role = %q, want %q", i, got, c.wantRoles[i])
				}
				switch normString(mm["role"]) {
				case "assistant":
					for _, tc := range normSlice(mm["tool_calls"]) {
						calls++
						callIDs[normString(normMap(tc)["id"])] = true
					}
				case "tool":
					results++
					resultIDs[normString(mm["tool_call_id"])] = true
				}
			}
			if calls != c.wantCalls || results != c.wantResult {
				t.Fatalf("calls=%d results=%d, want %d/%d", calls, results, c.wantCalls, c.wantResult)
			}
			// 对称性：每个残留 id 必须两侧都有。
			for id := range callIDs {
				if !resultIDs[id] {
					t.Fatalf("id %q 有调用无结果", id)
				}
			}
			for id := range resultIDs {
				if !callIDs[id] {
					t.Fatalf("id %q 有结果无调用", id)
				}
			}
		})
	}
}

// TestNormalizeDeepSeekThinking thinking 注入四种情形。
func TestNormalizeDeepSeekThinking(t *testing.T) {
	t.Run("未设置时注入 enabled + high", func(t *testing.T) {
		m := normRun(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, "", false)
		if got := normString(normMap(m["thinking"])["type"]); got != "enabled" {
			t.Fatalf("thinking.type = %q, want enabled", got)
		}
		if got := normString(m["reasoning_effort"]); got != "high" {
			t.Fatalf("reasoning_effort = %q, want high", got)
		}
	})
	t.Run("客户端 disabled 优先", func(t *testing.T) {
		m := normRun(t, `{"model":"deepseek-v4.1-flash","messages":[],"thinking":{"type":"disabled"}}`, "", false)
		if got := normString(normMap(m["thinking"])["type"]); got != "disabled" {
			t.Fatalf("thinking.type = %q, want disabled", got)
		}
		if _, ok := m["reasoning_effort"]; ok {
			t.Fatal("opted out 时不应注入 reasoning_effort")
		}
	})
	t.Run("客户端 effort none 优先", func(t *testing.T) {
		m := normRun(t, `{"model":"deepseek-v4.1-flash","messages":[],"reasoning_effort":"none"}`, "", false)
		if _, ok := m["thinking"]; ok {
			t.Fatal("effort=none 时不应注入 thinking")
		}
		if got := normString(m["reasoning_effort"]); got != "none" {
			t.Fatalf("reasoning_effort = %q, want none", got)
		}
	})
	t.Run("非 deepseek 不动", func(t *testing.T) {
		m := normRun(t, `{"model":"gpt-4o","messages":[],"thinking":{"type":"disabled"},"reasoning_effort":"high"}`, "", false)
		if _, ok := m["thinking"]; !ok {
			t.Fatal("非 deepseek 不应删除 thinking")
		}
		if got := normString(m["reasoning_effort"]); got != "high" {
			t.Fatalf("非 deepseek 不应改写 reasoning_effort, got %q", got)
		}
	})
}

// TestNormalizeReasoningBackfill 零痕迹历史与非字符串 reasoning_content。
func TestNormalizeReasoningBackfill(t *testing.T) {
	t.Run("零痕迹 + thinking 开启：reasoning 非空", func(t *testing.T) {
		m := normRun(t, `{"model":"deepseek-v4.1-flash","messages":[`+
			`{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`, "", false)
		asst := normMap(normTestMessages(t, m)[1])
		if _, ok := asst["reasoning_content"].(string); !ok {
			t.Fatalf("reasoning_content 应为字符串: %#v", asst["reasoning_content"])
		}
		if got := normString(asst["reasoning"]); got == "" {
			t.Fatal("reasoning 必须被镜像成非空值")
		}
	})
	t.Run("非字符串 reasoning_content 视为缺失", func(t *testing.T) {
		m := normRun(t, `{"model":"deepseek-v4.1-flash","messages":[`+
			`{"role":"user","content":"hi"},{"role":"assistant","content":"hello","reasoning_content":123}]}`, "", false)
		asst := normMap(normTestMessages(t, m)[1])
		if got := normString(asst["reasoning_content"]); got != "" {
			t.Fatalf("非字符串应回退为空串, got %q", got)
		}
		if got := normString(asst["reasoning"]); got == "" {
			t.Fatal("reasoning 必须为非空占位")
		}
	})
	t.Run("已有 reasoning 复用到 reasoning_content", func(t *testing.T) {
		m := normRun(t, `{"model":"deepseek-v4.1-flash","messages":[`+
			`{"role":"user","content":"hi"},{"role":"assistant","content":"hello","reasoning":"trace"}]}`, "", false)
		asst := normMap(normTestMessages(t, m)[1])
		if got := normString(asst["reasoning_content"]); got != "trace" {
			t.Fatalf("reasoning_content = %q, want trace", got)
		}
	})
	t.Run("非 deepseek 不回填", func(t *testing.T) {
		m := normRun(t, `{"model":"gpt-4o","messages":[{"role":"assistant","content":"hello"}]}`, "", false)
		asst := normMap(normTestMessages(t, m)[0])
		if _, ok := asst["reasoning_content"]; ok {
			t.Fatal("非 deepseek 不应回填 reasoning_content")
		}
	})
}

// TestNormalizeForceStream stream/stream_options 的设置与不覆盖。
func TestNormalizeForceStream(t *testing.T) {
	t.Run("设置 stream 与 include_usage", func(t *testing.T) {
		m := normRun(t, `{"model":"m","messages":[]}`, "", true)
		if m["stream"] != true {
			t.Fatalf("stream = %#v, want true", m["stream"])
		}
		if got := normMap(m["stream_options"])["include_usage"]; got != true {
			t.Fatalf("stream_options.include_usage = %#v, want true", got)
		}
	})
	t.Run("不覆盖调用方的 stream_options", func(t *testing.T) {
		m := normRun(t, `{"model":"m","messages":[],"stream_options":{"include_usage":false,"extra":1}}`, "", true)
		so := normMap(m["stream_options"])
		if so["include_usage"] != false || so["extra"] != float64(1) {
			t.Fatalf("stream_options 被改写: %#v", so)
		}
	})
}

// TestNormalizeInvalidJSON 不可解析时原样返回与 nil error。
func TestNormalizeInvalidJSON(t *testing.T) {
	cases := []string{`{not json`, `null`, `[1,2,3]`, ``}
	for _, c := range cases {
		body := []byte(c)
		// forceStream=true 跳过快路径，确保走到解析失败分支。
		out, err := NormalizeRequest(&model.Provider{}, "", body, true)
		if err != nil {
			t.Fatalf("body=%q err = %v, want nil", c, err)
		}
		if string(out) != c {
			t.Fatalf("body=%q 应原样返回, got %q", c, out)
		}
		if len(out) > 0 && &out[0] != &body[0] {
			t.Fatalf("body=%q 应返回同一底层切片", c)
		}
	}
}
