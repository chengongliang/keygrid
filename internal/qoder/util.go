package qoder

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

// util.go 小工具：UUID v4、JSON 序列化、时间解析。

// UUID 生成 v4 UUID（随机源不可用时退化为时间戳，保证不 panic）。
func UUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// nowUnixMillis 当前毫秒时间戳（business.begin_at 用）。
func nowUnixMillis() int64 { return time.Now().UnixMilli() }

// nowUnix 当前秒时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// jsonMarshal JSON 序列化：不转义 HTML（<、>、& 原样），
// 上游对转义差异不敏感，但保留原始字符更贴近 qodercli 流量。
func jsonMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
