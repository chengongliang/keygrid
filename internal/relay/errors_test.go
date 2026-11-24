package relay

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestGatewayUpstreamError(t *testing.T) {
	for _, entry := range []string{protoOpenAI, protoResponses, protoAnthropic} {
		t.Run(entry, func(t *testing.T) {
			upstream := &UpstreamError{
				Status: 400,
				Body:   `{"error":{"message":"Exact upstream\nmessage","type":"invalid_request_error","code":"context_length_exceeded","param":null},"request":"PRIVATE PROMPT"}`,
			}
			w := httptest.NewRecorder()
			if !gatewayUpstreamError(w, entry, upstream) || w.Code != 400 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var got, original map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(upstream.Body), &original); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got["error"], original["error"]) || got["request"] != nil {
				t.Fatalf("必须只返回原始 error 对象: %s", w.Body.String())
			}
			if entry == protoAnthropic && got["type"] != "error" {
				t.Fatalf("Anthropic 缺少错误信封: %s", w.Body.String())
			}
		})
	}
}

func TestGatewayUpstreamErrorFallback(t *testing.T) {
	for _, body := range []string{"", "<html>Bad Gateway</html>", `{"error":"token expired"}`, `{"error":null}`, `{"error":{}}`, `{"error":{"message":"truncated`} {
		w := httptest.NewRecorder()
		if gatewayUpstreamError(w, protoResponses, &UpstreamError{Status: 502, Body: body}) || w.Body.Len() != 0 {
			t.Fatalf("非结构化错误应由调用方兜底: %q → %s", body, w.Body.String())
		}
	}
}
