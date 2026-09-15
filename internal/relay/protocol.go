package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
)

// upstreamTimeout 上游转发 client 超时（对流式覆盖整个响应周期）。
const upstreamTimeout = 10 * time.Minute

// relayReq 一次转发请求的上下文（跨 failover 复用）。
type relayReq struct {
	Body     []byte
	Model    string
	UserID   int64
	APIKeyID int64
	IsStream bool
	w        http.ResponseWriter
	r        *http.Request
	h        *Handler
}

// UsageRecord usage 提取结果。
type UsageRecord struct {
	PromptTokens     int
	CompletionTokens int
	Model            string
	Found            bool
}

// extractUsageFromJSON 非流式响应 usage 提取。
func extractUsageFromJSON(data []byte) UsageRecord {
	var r struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Usage == nil {
		return UsageRecord{}
	}
	return UsageRecord{
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		Model:            r.Model,
		Found:            true,
	}
}

// streamPassthrough SSE 透传：逐 chunk 写回客户端，同时提取最后带 usage 的 chunk 记账。
// OpenAI 约定：`data: {...,"usage":{...}}` 行，最后一块 usage 非空；流以 `data: [DONE]` 结束。
func streamPassthrough(w http.ResponseWriter, upResp *http.Response) (int, UsageRecord, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, UsageRecord{}, errors.New("streaming unsupported by writer")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(upResp.StatusCode)

	var lastUsage UsageRecord
	buf := make([]byte, 32*1024)
	var pending []byte

	handleLine := func(line []byte) bool {
		// 返回 false 表示客户端断开
		if _, err := w.Write(append(line, '\n')); err != nil {
			return false
		}
		if ev, ok := parseSSELine(line); ok {
			if u, found := extractChunkUsage(ev); found {
				// 累积合并：openai 最后 chunk 带完整 usage；anthropic 的 input/output
				// tokens 分散在 message_start / message_delta 两个事件，取 max 归并
				if u.PromptTokens > lastUsage.PromptTokens {
					lastUsage.PromptTokens = u.PromptTokens
				}
				if u.CompletionTokens > lastUsage.CompletionTokens {
					lastUsage.CompletionTokens = u.CompletionTokens
				}
				if u.Model != "" {
					lastUsage.Model = u.Model
				}
				lastUsage.Found = true
			}
		}
		flusher.Flush()
		return true
	}

	for {
		n, err := upResp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				idx := indexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := pending[:idx]
				pending = pending[idx+1:]
				if !handleLine(line) {
					return upResp.StatusCode, lastUsage, errClientGone
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 残尾（无换行结尾）也要透传 + 提取
				if len(pending) > 0 {
					line := pending
					pending = nil
					if !handleLine(line) {
						return upResp.StatusCode, lastUsage, errClientGone
					}
				}
				return upResp.StatusCode, lastUsage, nil
			}
			return upResp.StatusCode, lastUsage, err
		}
	}
}

var errClientGone = errors.New("client gone")

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// parseSSELine 识别 "data: ..." 行，返回 JSON payload；[DONE]/空行返回 false。
func parseSSELine(line []byte) (json.RawMessage, bool) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	return json.RawMessage(payload), true
}

// extractChunkUsage 从 chunk JSON 提取 usage（usage 字段非空才算找到）。
// 兼容三种透传场景的 usage 形状：
//   - openai chunk：顶层 usage.prompt_tokens/completion_tokens
//   - responses completed：response.usage.input_tokens/output_tokens
//   - anthropic message_start/delta：message.usage 或顶层 usage 的 input_tokens/output_tokens
func extractChunkUsage(ev json.RawMessage) (UsageRecord, bool) {
	var m map[string]any
	if err := json.Unmarshal(ev, &m); err != nil {
		return UsageRecord{}, false
	}
	rec, found := usageFromAny(m["usage"])
	if !found {
		if resp, ok := m["response"].(map[string]any); ok {
			rec, found = usageFromAny(resp["usage"])
		}
		if !found {
			if msg, ok := m["message"].(map[string]any); ok {
				rec, found = usageFromAny(msg["usage"])
			}
		}
	}
	if !found {
		return UsageRecord{}, false
	}
	// model 依次从顶层 / response / message 取（透传时无 pivot 转换）
	if s, _ := m["model"].(string); s != "" {
		rec.Model = s
	} else if resp, ok := m["response"].(map[string]any); ok {
		if s, _ := resp["model"].(string); s != "" {
			rec.Model = s
		}
	} else if msg, ok := m["message"].(map[string]any); ok {
		if s, _ := msg["model"].(string); s != "" {
			rec.Model = s
		}
	}
	return rec, true
}

// credentialKey 解密凭据取 api_key。
func credentialKey(enc []byte) (string, error) {
	plain, err := crypto.Decrypt(enc)
	if err != nil {
		return "", err
	}
	var d struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(plain, &d); err != nil || d.APIKey == "" {
		return "", errors.New("invalid credential data")
	}
	return d.APIKey, nil
}
