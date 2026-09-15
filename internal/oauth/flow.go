package oauth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// flow.go authorization_code 变体流程支持（iFlow 等）。
//
// 现有两种流程：
//   - device_code: BeginAuth 拿 user_code → 前端轮询 Resolve
//   - pkce:        BeginAuth 拼 authorize URL → 浏览器回调 /api/oauth/callback 落 code → 前端 poll Resolve
//
// auth_code 与 pkce 的宿主侧交互完全一致（跳转 + 回调 + poll），差别只在
// adapter 内部换 token 的方式（Basic Auth / 自定义 body / 无 code_verifier），
// 因此不新增宿主状态机，只加一个 FlowType 标识。

const FlowAuthCode FlowType = "auth_code"

// ResponseAuthError 上游在浏览器回调里带回了授权错误（error=access_denied 等）。
// Callback handler 据此返回 400 而非静默成功。
type ResponseAuthError struct {
	ErrCode string
	Desc    string
}

func (e *ResponseAuthError) Error() string {
	if e.Desc != "" {
		return "oauth response error: " + e.ErrCode + " (" + e.Desc + ")"
	}
	return "oauth response error: " + e.ErrCode
}

// CaptureCallbackParams 回调参数落 state.temp 的统一键名。
// 各 adapter 在 Resolve 里按自己的形状解析 rawCallback。
const (
	CallbackParamRaw   = "_rawCallback" // 完整 query 串（adapter 自行解析）
	CallbackParamError = "_error"       // 上游回调带回的错误码
	CallbackParamDesc  = "_error_desc"  // 错误描述
)

// ExtractCallbackError 从宿主写入的 temp 提取回调错误（adapter Resolve 开头调用）。
// 返回 nil 表示回调正常。
func ExtractCallbackError(temp map[string]string) *ResponseAuthError {
	if temp == nil || temp[CallbackParamError] == "" {
		return nil
	}
	return &ResponseAuthError{ErrCode: temp[CallbackParamError], Desc: temp[CallbackParamDesc]}
}

// JSONPath 按 path 依次取嵌套 map 值，返回字符串（不存在/类型不符返回 ""）。
// path 为 ["Result","LoginHost"] 表示 data["Result"]["LoginHost"]。
func JSONPath(data any, path ...string) string {
	var cur any = data
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

// ParseJSONObject 从 JSON 文本解析顶层 object（失败返回 nil）。
func ParseJSONObject(body string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil
	}
	return m
}

// http import 占位 —— 保持与 types.go 一致的导入面。
var _ = http.MethodGet

// trimSpace 小工具：回调参数 trim（避免各 adapter 重复 import strings）。
func trimSpace(s string) string { return strings.TrimSpace(s) }
