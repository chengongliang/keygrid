package relay

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// workbuddy_runtime.go relay ↔ workbuddy 运行态的桥接辅助。
//
// 上游协议差异（Retry-After、业务错误码）在 relay 侧抽取，workbuddy 包只接收
// 结构化事件，避免把上游响应解析塞进业务包。

// retryAfterFrom 取上游错误里的 Retry-After 退避值（0 = 未提供）。
func retryAfterFrom(err error) time.Duration {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	return 0
}

// upstreamCodeFromError 从上游错误体里提取业务错误码（如 WorkBuddy 的 6004 限额）。
//
// 只读数值型 code / error.code / status 三个字段；正文本身绝不落日志。
// 解析失败返回 0（"未知"），调用方只把它当作参考信号。
func upstreamCodeFromError(err error) int {
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Body == "" {
		return 0
	}
	var obj map[string]any
	if json.Unmarshal([]byte(ue.Body), &obj) != nil {
		return 0
	}
	if v, ok := jsonInt(obj["code"]); ok {
		return v
	}
	if inner, ok := obj["error"].(map[string]any); ok {
		if v, ok := jsonInt(inner["code"]); ok {
			return v
		}
	}
	if v, ok := jsonInt(obj["status"]); ok {
		return v
	}
	return 0
}

// jsonInt 把 JSON 数字/数字字符串转成 int（其余类型视为不可转换）。
func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		if out, err := strconv.Atoi(n); err == nil {
			return out, true
		}
	}
	return 0, false
}
