package workbuddy

// identity.go 复刻 Python 网关的「请求身分头」。
//
// 背景：上游 WAF 会拒绝缺少 IDE 身分头的请求，错误码 11128
// （illegal API invocation from an unapproved channel）。因此出站请求必须带上
// 一套与区域、产品档位相匹配的 UA / X-IDE-* / X-Domain 头。
//
// 对齐 wb2a 的事实来源：
//   - wb_identity.py:44-53（区域域名）、wb_identity.py:57（workbuddy.cn）
//   - wb_identity.py 的 UA / 版本表（desktop / vscode / cli 三档）
//   - Python 源码中 X-Machine-ID / X-Session-ID 由设备指纹推导；本项目没有设备
//     指纹，改用 uid 的稳定派生值（见下），故默认关闭。

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/workbuddy/upstream"
)

// 产品档位常量。出站身分三选一，未知值回落到 workbuddy（桌面端）。
const (
	productDesktop = "workbuddy"
	productVSCode  = "vscode"
	productCLI     = "cli"
)

// 导出别名：settings.product 的合法取值（server/handlers 校验用，同一值域）。
const (
	ProductWorkbuddy = productDesktop
	ProductVSCode    = productVSCode
	ProductCLI       = productCLI
)

// 区域域名（身分头里的 X-Domain / Origin / Referer）。
// 对齐 wb_identity.py:44-53：国际版 www.workbuddy.ai，国内版 copilot.tencent.com。
const (
	intlIdentityDomain = "www.workbuddy.ai"
	cnIdentityDomain   = "copilot.tencent.com"
)

// HeaderOpt 控制身分头的可选部分。
type HeaderOpt struct {
	// Product 出站身分档位：workbuddy（默认，桌面端）/ vscode / cli。
	Product string
	// Fingerprint 是否附带设备指纹头（X-Machine-ID / X-Session-ID），默认 false。
	Fingerprint bool
	// ConversationID 会话 id；空则由本函数生成。
	ConversationID string
	// Now 注入当前时间（测试用）；nil 用 time.Now。
	Now func() time.Time
}

// DefaultProduct 返回默认身分档位（桌面端 workbuddy）。
func DefaultProduct() string { return productDesktop }

// Headers 返回鉴权之外的上游请求头。
//
// 调用方（relay）负责设置 Authorization / Content-Type / Accept，本函数不设置
// Authorization。返回的 User-Agent 是"身分 UA"，调用方应优先使用它而不是透传客户端 UA。
func Headers(p *model.Provider, tok *oauth.TokenSet, opt HeaderOpt) (http.Header, error) {
	realm, ok := RealmOf(p)
	if !ok {
		return nil, ErrNotWorkBuddy
	}

	product := identProduct(opt.Product)
	domain := identDomain(realm)
	if realm == RealmCN && product == productVSCode {
		domain = "www.workbuddy.cn"
	}
	if tok != nil && strings.TrimSpace(tok.Extra["domain"]) != "" {
		domain = strings.TrimSpace(tok.Extra["domain"])
	}
	uid := UIDFrom(tok)

	h := make(http.Header)
	// 固定部分：三个产品档一致。
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-CodeBuddy-Request", "1")
	h.Set("Origin", "https://"+domain)
	h.Set("Referer", "https://"+domain+"/")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")

	// 三个产品档都有的公共身分头。
	h.Set("X-Domain", domain)
	h.Set("X-IDE-Version", identVersion(realm, product))
	h.Set("X-User-Id", uid)
	h.Set("User-Agent", uaFor(realm, product))

	now := opt.Now
	if now == nil {
		now = time.Now
	}
	gen := &identGen{now: now}

	switch product {
	case productVSCode:
		h.Set("X-IDE-Name", "VSCode")
		h.Set("X-IDE-Type", "VSCode")
		h.Set("X-Product", "SaaS")
		h.Set("X-Agent-Intent", "craft")
		// vscode 专属：环境标识与产品版本。
		h.Set("X-Env-ID", "production")
		h.Set("X-Product-Version", upstream.VSCodeProductVersion)
		h.Set("X-Agent-Type", "main")
		if realm == RealmIntl {
			h.Set("X-Private-Data", "true")
		}
	case productCLI:
		h.Set("X-IDE-Name", "CLI")
		h.Set("X-IDE-Type", "CLI")
		h.Set("X-Product", "SaaS")
		h.Set("X-Agent-Intent", "craft")
		h.Set("X-Agent-Purpose", "conversation")
		h.Set("X-Agent-Type", "main")
		h.Set("X-Agent-Runtime", "other")
		privateData := "false"
		if cnIsEnterprise(tok) {
			privateData = "true"
		}
		h.Set("X-Private-Data", privateData)
	default: // productDesktop
		h.Set("X-IDE-Name", "WorkBuddy")
		h.Set("X-IDE-Type", "WorkBuddy")
		h.Set("X-Product", "WorkBuddy")
		h.Set("X-Agent-Purpose", "conversation")
		h.Set("X-Agent-Type", "main")
		h.Set("X-Agent-Runtime", "other")
	}

	convID := strings.TrimSpace(opt.ConversationID)
	if convID == "" {
		convID = gen.next()
		if product != productVSCode {
			convID = convID[:8] + "-" + convID[8:12] + "-" + convID[12:16] + "-" + convID[16:20] + "-" + convID[20:]
		}
	}
	h.Set("X-Conversation-ID", convID)
	requestID, messageID := gen.next(), gen.next()
	h.Set("X-Conversation-Request-ID", requestID)
	h.Set("X-Conversation-Message-ID", messageID)
	if product == productVSCode {
		h.Set("X-Request-ID", requestID)
	} else {
		h.Set("X-Root-Request-ID", requestID)
		h.Set("X-Request-ID", messageID)
	}

	// 设备指纹头：Python 源从真实设备指纹推导；我们只有 uid，改用稳定但合成的
	// 派生值，因此默认关闭（opt.Fingerprint=false），需要时由调用方显式打开。
	if opt.Fingerprint {
		h.Set("X-Machine-ID", identFingerprint("keygrid:workbuddy:machine:", uid))
		h.Set("X-Session-ID", identFingerprint("keygrid:workbuddy:session:", uid))
	}

	return h, nil
}

// UIDFrom 取账号 uid：优先 tok.Extra["uid"]，否则从未校验的 JWT payload 的
// "uid" claim 取（仅用于请求头，不做签名校验）。
func UIDFrom(tok *oauth.TokenSet) string {
	if tok == nil {
		return ""
	}
	if uid := strings.TrimSpace(tok.Extra["uid"]); uid != "" {
		return uid
	}
	return identUIDFromJWT(tok.AccessToken)
}

// identProduct 归一化产品档位，未知值回落到桌面端（不报错）。
func identProduct(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case productVSCode:
		return productVSCode
	case productCLI:
		return productCLI
	default:
		return productDesktop
	}
}

// identDomain 区域对应的身分域名。
func identDomain(realm Realm) string {
	if realm == RealmCN {
		return cnIdentityDomain
	}
	return intlIdentityDomain
}

// identVersion X-IDE-Version，按区域 + 产品档取值。
func identVersion(realm Realm, product string) string {
	switch product {
	case productVSCode:
		return upstream.VSCodeVersion
	case productCLI:
		return upstream.CLIVersion
	default:
		if realm == RealmCN {
			return upstream.DesktopVersionCN
		}
		return upstream.DesktopVersionIntl
	}
}

// uaFor 身分 User-Agent，按区域 + 产品档取值。
func uaFor(realm Realm, product string) string {
	switch product {
	case productVSCode:
		return upstream.VSCodeUA
	case productCLI:
		return upstream.CLIUA
	default: // desktop
		if realm == RealmCN {
			return upstream.DesktopUACN
		}
		return upstream.DesktopUAIntl
	}
}

// identGen 生成带随机位的 UUIDv7，请求 id 使用无连字符的 32 位 hex。
type identGen struct {
	now func() time.Time
}

func (g *identGen) next() string {
	var identifier [16]byte
	_, _ = rand.Read(identifier[:])
	millis := uint64(g.now().UnixMilli())
	for index := 5; index >= 0; index-- {
		identifier[index] = byte(millis)
		millis >>= 8
	}
	identifier[6] = identifier[6]&0x0f | 0x70
	identifier[8] = identifier[8]&0x3f | 0x80
	return hex.EncodeToString(identifier[:])
}

// identFingerprint 对 prefix+payload 取 md5 的 32 位小写 hex。
func identFingerprint(prefix, payload string) string {
	sum := md5.Sum([]byte(prefix + payload))
	return hex.EncodeToString(sum[:])
}

// identUIDFromJWT 从未校验的 JWT payload 里读 "uid" claim；解析失败返回空串。
func identUIDFromJWT(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, ok := identDecodeSegment(parts[1])
	if !ok {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if uid, ok := claims["uid"].(string); ok {
		return strings.TrimSpace(uid)
	}
	return ""
}

// identDecodeSegment base64url 解码 JWT 片段，兼容带/不带 padding 与 -/_ 字母表。
func identDecodeSegment(seg string) ([]byte, bool) {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return nil, false
	}
	if b, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return b, true
	}
	if b, err := base64.URLEncoding.DecodeString(seg); err == nil {
		return b, true
	}
	// 手动补齐 padding 再试一次。
	if m := len(seg) % 4; m != 0 {
		padded := seg + strings.Repeat("=", 4-m)
		if b, err := base64.URLEncoding.DecodeString(padded); err == nil {
			return b, true
		}
	}
	return nil, false
}
