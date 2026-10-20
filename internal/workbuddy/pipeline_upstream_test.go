package workbuddy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeToolParameters(t *testing.T) {
	cases := []struct {
		name       string
		parameters string
		want       string
	}{
		{"无参工具", `{}`, `{"type":"object","properties":{}}`},
		{"null", `null`, `{"type":"object","properties":{}}`},
		{"错误形状", `[]`, `{"type":"object","properties":{}}`},
		{"清理 required", `{"required":[null,"",false,1,"field"]}`, `{"type":"object","properties":{},"required":["field"]}`},
		{"字符串 required", `{"required":" field "}`, `{"type":"object","properties":{},"required":["field"]}`},
		{"空 required", `{"required":false}`, `{"type":"object","properties":{}}`},
		{"嵌套别名", `{"properties":{"value":{"type":"int","description":null},"data":{"type":"dict"},"list":{"type":"list","items":[null,{"type":"str"}]}}}`,
			`{"type":"object","properties":{"value":{"type":"integer"},"data":{"type":"object","properties":{}},"list":{"type":"array","items":{"type":"string"}}}}`},
		{"定义和组合", `{"$defs":{"value":{"type":"float"}},"definitions":{"x":{"type":"bool"}},"properties":{"value":{"anyOf":[{"type":"text"},{"type":"null"}],"oneOf":[{"type":"int"}],"allOf":[{"type":"dict"}]}}}`,
			`{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"null"}],"oneOf":[{"type":"integer"}],"allOf":[{"type":"object","properties":{}}]}},"$defs":{"value":{"type":"number"}},"definitions":{"x":{"type":"boolean"}}}`},
		{"合法约束保持", `{"type":"object","properties":{"nullable":{"type":["string","null"],"default":null},"value":{"$ref":"#/$defs/value","enum":["a","b"]}},"additionalProperties":false,"required":["value"]}`,
			`{"type":"object","properties":{"nullable":{"type":["string","null"],"default":null},"value":{"$ref":"#/$defs/value","enum":["a","b"]}},"additionalProperties":false,"required":["value"]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := normRun(t, `{"model":"m","tools":[{"type":"function","function":{"name":"test","strict":true,"parameters":`+testCase.parameters+`}}]}`, "m", false)
			function := normMap(normMap(normSlice(result["tools"])[0])["function"])
			var want map[string]any
			if err := json.Unmarshal([]byte(testCase.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(function["parameters"], want) || function["strict"] != true {
				t.Fatalf("归一化结果 = %#v, want %#v", function, want)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if again := normRun(t, string(encoded), "m", false); !reflect.DeepEqual(result, again) {
				t.Fatalf("归一化不幂等: %#v", again)
			}
		})
	}
}

func TestNormalizeLegacyFunctions(t *testing.T) {
	result := normRun(t, `{"model":"m","functions":[null,{"name":"legacy","parameters":{}}]}`, "m", false)
	if _, exists := result["functions"]; exists {
		t.Fatal("旧版 functions 应转为 tools")
	}
	tools := normSlice(result["tools"])
	if len(tools) != 1 || normMap(normMap(tools[0])["function"])["name"] != "legacy" {
		t.Fatalf("转换结果错误: %#v", tools)
	}
	for _, raw := range []string{`[]`, `[null,"invalid"]`, `null`, `"invalid"`} {
		if result := normRun(t, `{"model":"m","tools":`+raw+`}`, "m", false); result["tools"] != nil {
			t.Fatalf("空工具列表应删除: %#v", result)
		}
	}
}

func TestNormalizeToolsMissingType(t *testing.T) {
	for _, declaration := range []string{`{"name":"flat","parameters":{}}`, `{"function":{"name":"nested","parameters":{}}}`} {
		result := normRun(t, `{"model":"m","tools":[`+declaration+`]}`, "m", false)
		tool := normMap(normSlice(result["tools"])[0])
		if tool["type"] != "function" || normMap(normMap(tool["function"])["parameters"])["type"] != "object" {
			t.Fatalf("缺省类型未修复: %#v", tool)
		}
	}
}

func TestNormalizeToolArgumentsAndIntruders(t *testing.T) {
	result := normRun(t, `{"model":"m","messages":[
		{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"test","arguments":{"value":"<你好>"}}},{"id":"b","type":"function","function":{"name":"test","arguments":"{\"value\":2}"}}]},
		{"role":"user","content":"插入消息"},
		{"role":"tool","tool_call_id":"a","content":"result a"},
		{"role":"developer","content":"插入指令"},
		{"role":"tool","tool_call_id":"b","content":"result b"},
		{"role":"assistant","content":"完成"}
	]}`, "m", false)
	messages := normTestMessages(t, result)
	var roles []string
	for _, raw := range messages {
		roles = append(roles, normString(normMap(raw)["role"]))
	}
	if want := []string{"assistant", "tool", "tool", "user", "system", "assistant"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("顺序 = %v, want %v", roles, want)
	}
	calls := normSlice(normMap(messages[0])["tool_calls"])
	if got := normMap(normMap(calls[0])["function"])["arguments"]; got != `{"value":"<你好>"}` {
		t.Fatalf("对象参数未编码成字符串: %#v", got)
	}
	if got := normMap(normMap(calls[1])["function"])["arguments"]; got != `{"value":2}` {
		t.Fatalf("字符串参数不应改变: %#v", got)
	}
}

func TestToolRepackStopsAtAssistant(t *testing.T) {
	messages := normTestParseMsgs(t, `[
		{"role":"assistant","tool_calls":[{"id":"a"},{"id":"b"}]},
		{"role":"tool","tool_call_id":"a","content":"result"},
		{"role":"assistant","content":"新轮次"},
		{"role":"tool","tool_call_id":"b","content":"late"}
	]`)
	if _, changed := toolRepackResults(messages); changed {
		t.Fatal("工具结果不得跨 assistant 轮次重排")
	}
}
