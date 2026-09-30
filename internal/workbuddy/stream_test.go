package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// stream_test.go 强制流式下 SSE → 单条 JSON 的折叠（G3 的回归测试）。

const sseSample = `data: {"id":"c1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"先想"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"SH\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}

data: [DONE]

`

func TestCollapseSSEAccumulatesEverything(t *testing.T) {
	out, ok := CollapseSSE([]byte(sseSample))
	if !ok {
		t.Fatal("CollapseSSE() ok=false, want true")
	}
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("折叠结果不是合法 JSON: %v (%s)", err, out)
	}
	if got.Object != "chat.completion" || got.Model != "deepseek-v4.1-flash" {
		t.Fatalf("object/model 不对: %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices 数 = %d, want 1", len(got.Choices))
	}
	msg := got.Choices[0].Message
	if msg.Role != "assistant" || msg.Content != "Hello" {
		t.Fatalf("message = %+v", msg)
	}
	if msg.ReasoningContent != "先想" {
		t.Fatalf("reasoning_content = %q", msg.ReasoningContent)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Fatalf("tool_call 头部 = %+v", tc)
	}
	if tc.Function.Arguments != `{"city":"SH"}` {
		t.Fatalf("分片参数未拼接: %q", tc.Function.Arguments)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", got.Choices[0].FinishReason)
	}
	if got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 7 || got.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestCollapseSSEPassthroughCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"空输入", ""},
		{"直接返回 JSON", `{"id":"x","choices":[{"message":{"content":"hi"}}]}`},
		{"只有 DONE", "data: [DONE]\n\n"},
		{"只有逗号分隔的注释/心跳", ": ping\n\n: ping\n\n"},
		{"流里只有上游错误", "data: {\"error\":{\"message\":\"boom\"}}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := CollapseSSE([]byte(tc.in)); ok {
				t.Fatalf("CollapseSSE(%q) ok=true, want false（应原样透传）", tc.in)
			}
		})
	}
}

func TestCollapseSSEToleratesCRLFAndBadChunks(t *testing.T) {
	in := "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\r\n\r\n" +
		"data: {broken json\r\n\r\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	out, ok := CollapseSSE([]byte(in))
	if !ok {
		t.Fatal("CRLF 流应当能折叠")
	}
	if !strings.Contains(string(out), `"content":"ab"`) {
		t.Fatalf("坏块不应中断折叠: %s", out)
	}
	if !strings.Contains(string(out), `"prompt_tokens":3`) {
		t.Fatalf("usage 未保留: %s", out)
	}
}

func TestCollapseSSEUsageOnlyStream(t *testing.T) {
	in := "data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":0,\"total_tokens\":1}}\n\n"
	out, ok := CollapseSSE([]byte(in))
	if !ok {
		t.Fatal("只有 usage 的流也应产出响应（否则记账丢失）")
	}
	if !strings.Contains(string(out), `"content":""`) {
		t.Fatalf("空内容应以空字符串表达: %s", out)
	}
}

func TestCollapseSSEArrayContentParts(t *testing.T) {
	in := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"分\"},{\"type\":\"text\",\"text\":\"段\"}]}}]}\n\n"
	out, ok := CollapseSSE([]byte(in))
	if !ok {
		t.Fatal("数组形态 content 应当能折叠")
	}
	if !strings.Contains(string(out), `"content":"分段"`) {
		t.Fatalf("数组 content 未拼接: %s", out)
	}
}
