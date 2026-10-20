package relay

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
)

// upstream_headers.go 渠道级上游附加请求头（providers.upstream_headers）。
//
// 为什么需要：部分上游（Grok Build / Codex / Trae …）用客户端指纹头做准入校验，
// 版本号写死在代码里，上游抬高最低版本后只能改代码重新发版。渠道级附加头让
// 这类头在 UI 上可调，出问题时不发版即可跟进。
//
// 安全边界（与 AGENTS 硬性规则一致）：
//   - 只作用于渠道 owner 自己的转发请求（渠道已按 user_id 隔离），不引入跨用户能力；
//   - 黑名单拒绝覆盖鉴权 / 路由 / 报文形状相关的头（见 forbiddenUpstreamHeaders）——
//     否则用户可绕过平台注入的凭据，或伪造 Host 打内网（SSRF 侧已另有一层校验）；
//   - User-Agent 走渠道 ua_mode 语义，不允许从这里改（避免两套机制冲突）；
//   - 头名/值做 token 与 CR/LF 校验，防 header 注入；
//   - 头值不写日志、不进审计详情（可能含上游下发的一次性标识）。

const (
	// maxUpstreamHeaderCount 附加头条目上限（防御性限制）
	maxUpstreamHeaderCount = 20
	// maxUpstreamHeaderNameLen 头名长度上限
	maxUpstreamHeaderNameLen = 64
	// maxUpstreamHeaderValueLen 头值长度上限
	maxUpstreamHeaderValueLen = 1024
)

// forbiddenUpstreamHeaders 禁止通过渠道附加头覆盖的头 → 拒绝原因（错误提示用）。
// key 为小写规范化名。
var forbiddenUpstreamHeaders = map[string]string{
	"authorization":             "鉴权头由平台按渠道凭据注入",
	"proxy-authorization":       "代理鉴权头由平台管理",
	"cookie":                    "Cookie 不在网关转发语义内",
	"set-cookie":                "Cookie 不在网关转发语义内",
	"host":                      "Host 由目标地址决定（防伪造路由/SSRF）",
	"content-length":            "报文长度由平台按实际 body 计算",
	"transfer-encoding":         "传输编码由 HTTP 客户端决定",
	"connection":                "连接控制头由 HTTP 客户端决定",
	"upgrade":                   "连接控制头由 HTTP 客户端决定",
	"te":                        "连接控制头由 HTTP 客户端决定",
	"trailer":                   "连接控制头由 HTTP 客户端决定",
	"proxy-connection":          "连接控制头由 HTTP 客户端决定",
	"content-type":              "Content-Type 由平台按入口协议决定",
	"user-agent":                "User-Agent 请用渠道的 UA 策略（ua_mode）配置",
	"x-api-key":                 "鉴权头由平台按渠道凭据注入",
	"x-xai-token-auth":          "xAI 身份头由平台按渠道类型注入",
	"x-authenticateresponse":    "xAI 身份头由平台按渠道类型注入",
	"chatgpt-account-id":        "Codex 账号头来自凭据，不能由渠道覆盖",
	"openai-organization":       "组织头不属于客户端指纹，禁止覆盖",
	"openai-beta":               "协议特性头由平台按入口协议决定",
	"anthropic-beta":            "协议特性头由平台按入口协议决定",
	"anthropic-version":         "协议版本头由平台按入口协议决定",
	"accept":                    "Accept 由平台按流式/非流式决定",
	"originator":                "Codex originator 与下游 UA 策略交叉校验，禁止单独覆盖",
	"x-stainless-arch":          "SDK 指纹头不在可覆盖范围",
	"x-stainless-lang":          "SDK 指纹头不在可覆盖范围",
	"x-stainless-os":            "SDK 指纹头不在可覆盖范围",
	"x-stainless-package-ver":   "SDK 指纹头不在可覆盖范围",
	"x-stainless-retry-count":   "SDK 指纹头不在可覆盖范围",
	"x-stainless-runtime":       "SDK 指纹头不在可覆盖范围",
	"x-stainless-runtime-ver":   "SDK 指纹头不在可覆盖范围",
	"x-stainless-timeout":       "SDK 指纹头不在可覆盖范围",
	"x-stainless-helper-method": "SDK 指纹头不在可覆盖范围",
}

// NormalizeUpstreamHeaders 规整头名/头值（去首尾空白、丢弃空条目），返回新 map。
// 头名保留用户书写形式（http.Header.Set 内部会再规范化），比较一律小写。
func NormalizeUpstreamHeaders(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		name := strings.TrimSpace(k)
		value := strings.TrimSpace(v)
		if name == "" && value == "" {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ValidUpstreamHeaders 校验渠道附加头：数量、头名合法性（RFC 7230 token）、
// 头值（禁 CR/LF、长度）、黑名单。返回 nil 表示可落库。
func ValidUpstreamHeaders(m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	if len(m) > maxUpstreamHeaderCount {
		return fmt.Errorf("upstream_headers: 最多 %d 条", maxUpstreamHeaderCount)
	}
	// 排序后校验，保证多条非法时错误提示稳定（便于前端/测试断言）
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		value := strings.TrimSpace(m[raw])
		if !validHeaderName(name) {
			return fmt.Errorf("upstream_headers: 非法头名 %q", raw)
		}
		lower := strings.ToLower(name)
		if reason, banned := forbiddenUpstreamHeaders[lower]; banned {
			return fmt.Errorf("upstream_headers: 不允许覆盖 %s（%s）", name, reason)
		}
		if len(value) > maxUpstreamHeaderValueLen {
			return fmt.Errorf("upstream_headers: %s 的值超过 %d 字符", name, maxUpstreamHeaderValueLen)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("upstream_headers: %s 的值不能包含换行", name)
		}
	}
	return nil
}

// validHeaderName RFC 7230 token 校验（字符集 + 长度）。
func validHeaderName(name string) bool {
	if name == "" || len(name) > maxUpstreamHeaderNameLen {
		return false
	}
	const tcharSpecial = "!#$%&'*+-.^_`|~"
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(tcharSpecial, c) >= 0:
		default:
			return false
		}
	}
	return true
}

// ApplyUpstreamHeaders 把渠道附加头落到本次上游请求上（覆盖同名头）。
// 平台/身份头先写入，再调用本函数 —— 用户显式配置优先；黑名单条目在写库前
// 已拒绝（存量脏数据在此静默跳过，绝不因一条坏配置让请求发不出去）。
// 转发主路径与渠道测连（providertest）共用，保证两边看到的身份头一致。
func ApplyUpstreamHeaders(h http.Header, p *model.Provider) {
	if h == nil || p == nil || len(p.UpstreamHeaders) == 0 {
		return
	}
	for name, value := range p.UpstreamHeaders {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !validHeaderName(name) || strings.ContainsAny(value, "\r\n") {
			continue
		}
		if _, banned := forbiddenUpstreamHeaders[strings.ToLower(name)]; banned {
			continue
		}
		h.Set(name, value)
	}
}
