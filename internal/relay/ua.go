package relay

import (
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
)

// ua.go 上游 User-Agent 策略。
//
// 默认（渠道未配置）透传客户端 UA：上游看到的是真实客户端（pi-agent / codex /
// claude / 各家 SDK），便于上游侧排查与风控对齐；也避免所有网关流量都以
// 同一个 UA（Go-http-client/1.1）出现而被上游按指纹限流。
//
// Codex 渠道例外：ChatGPT 后端强依赖 codex_cli_rs 与 originator 头（见
// codexUpstreamHeaders），默认保持调用点自带的固定值，渠道上可显式改为
// forward / custom 覆盖。
const (
	// UAModeCustom 用渠道自定义 UA（providers.user_agent）
	UAModeCustom = "custom"
	// UAModeForward 强制透传客户端 UA（含 Codex 渠道）
	UAModeForward = "forward"

	// maxUserAgentLen 自定义 UA 长度上限（正常 UA 不会超过几十字符，防御性限制）
	maxUserAgentLen = 256
)

// ResolveUserAgent 计算本次上游请求应发送的 User-Agent；空串 = 不设置该头
// （交给调用点原行为：Codex 用固定 UA，其余走 Go http 客户端默认值）。
//
// clientUA 来自入口请求（可能为空：客户端未带 UA 时不做任何伪装）。
func ResolveUserAgent(p *model.Provider, proto string, clientUA string) string {
	clientUA = sanitizeUAValue(clientUA)
	if p != nil {
		switch p.UAMode {
		case UAModeCustom:
			if v := sanitizeUAValue(p.UserAgent); v != "" {
				return v
			}
		case UAModeForward:
			return clientUA
		}
	}
	// 默认策略：Codex 渠道保持调用点的固定 UA（返回空 = 不改写）
	if proto == protoResponses {
		return ""
	}
	return clientUA
}

// sanitizeUAValue 规整 UA 取值：去除首尾空白、丢掉含 CR/LF 的脏值（防 header 注入）。
func sanitizeUAValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, "\r\n") {
		return ""
	}
	return v
}

// ValidUserAgent 校验渠道自定义 UA：非空、无 CR/LF（防 header 注入）、长度受限。
func ValidUserAgent(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxUserAgentLen {
		return false
	}
	return !strings.ContainsAny(v, "\r\n")
}

// ValidUAMode 校验渠道 UA 策略取值（"" = 默认）。
func ValidUAMode(m string) bool {
	switch m {
	case "", UAModeCustom, UAModeForward:
		return true
	}
	return false
}
