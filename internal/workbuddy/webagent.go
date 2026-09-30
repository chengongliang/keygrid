package workbuddy

// webagent.go INTL 网页通道 ACP 打卡（设计文档 §3.7「ACP 网页通道打卡」，
// `daily_chat_web` 行）。
//
// 移植自 wb2a 的 wb_webagent.py + wb_accounts.py:604-685。要点（五步）：
//
//  1. POST {intl}/console/as/conversations/ → 拿 conversation id（只排队，不算打卡）；
//  2. GET  {intl}/console/as/conversations/{id}/session → {link, token, sessionId, cwd}；
//  3. GET  {link}  Accept: text/event-stream → 响应头取 Acp-Connection-Id；
//  4. POST {link}  header Acp-Connection-Id → JSON-RPC：initialize(protocolVersion=1,
//     客户端能力全关) → session/load(sessionId, cwd, mcpServers:[]) → session/prompt；
//  5. 后台读 SSE（统计 session/update / agent_message_chunk 段数）+ 轮询会话 status，
//     completed 即成功，failed/error 停止。
//
// ★v1.6.9 的坑★（wb_webagent.py:4-5、wb_accounts.py:646）：创建会话**只是排队**，
// 不接沙箱、不请求这一轮，会话会永远停在 CREATING，打卡不计入。因此本函数只有在
// 轮询到 `completed`（即沙箱这一轮真的跑完）后才返回 ok；仅凭「建会话成功」绝不算
// 成功。这是本文件最重要的正确性约束，勿当冗余删掉。
//
// 红线：日志与 Message 只允许出现状态字、计数、耗时、结构化上游错误码；绝不出现
// prompt、SSE 正文、token 或响应体原文。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// 网页通道固定协议参数（对齐 wb_accounts.py:35-42、wb_webagent.py:25-31）。
const (
	// webProtocolVersion ACP 协议版本（wb_webagent.py:25）。
	webProtocolVersion = 1
	// webPromptText 打卡用的短 prompt（wb_accounts.py:40 DAILY_CHAT_WEB_PROMPT）。
	webPromptText = "Hi"
	// webDefaultModel 低耗打卡模型（wb_accounts.py:39 DAILY_CHAT_MODEL）。
	webDefaultModel = "deepseek-v4.1-flash"
	// webConversationOrigin / 插件项：网页端抓包所得，保持请求形态一致
	// （wb_accounts.py:617-623）。
	webConversationOrigin = "workbuddy-app"
	webPluginName         = "weixinpay"
	webPluginMarketplace  = "codebuddy-builtin"
	// webUserAgent 网页端 UA（wb_accounts.py:37-38 WEB_USER_AGENT）。
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"

	// webTurnTimeoutEnv 单轮超时环境变量；数值可为 Go duration（如 90s）或整数秒。
	webTurnTimeoutEnv = "WORKBUDDY_WEB_TURN_TIMEOUT"
	// webDefaultTurnTimeout 单轮默认超时 120s（wb_accounts.py:42 WB_WEB_TURN_TIMEOUT）。
	// 注：任务框架在 registry 里还会给一个外层 120s 超时，那是第二道保险；本函数
	// 内部必须有自己的上限，不能只依赖外层。
	webDefaultTurnTimeout = 120 * time.Second

	// webMetaKeyLastConversation 最后一条跑完的会话 id 存进
	// WorkbuddyChannelState.Meta 的命名空间键。
	webMetaKeyLastConversation = "acpLastConversationID"

	// webErrTextLen 失败摘要里错误文本的截断上限。
	webErrTextLen = 120
)

// ---- 测试接缝（package-level，测试可替换；生产等价于直接调用） ----

var (
	// webBaseURL 网页通道基址构造（测试指向 httptest；wb2a 里等价 WEB_ORIGIN）。
	webBaseURL = WebBase
	// webPollInterval 状态轮询间隔（wb_webagent.py 默认 3s；测试调短）。
	webPollInterval = 3 * time.Second
	// webSSEReaderExited SSE 读取协程退出时调用（生产 no-op；测试用它断言不泄漏）。
	webSSEReaderExited = func() {}
	// webMetaWarnOnce 保证元数据持久化失败整个进程只告警一次，避免刷屏。
	webMetaWarnOnce sync.Once
)

// runDailyChatWebJob INTL 网页通道 ACP 打卡（设计文档 §3.7）。
// 单轮超时由任务框架给（registry 里配 120s），本函数内部还有自己的 SSE 读取上限。
func runDailyChatWebJob(ctx context.Context, jc *JobContext) (status string, message string) {
	if jc == nil || jc.Provider == nil || jc.Runtime == nil {
		return jobStatusFailed, "任务上下文缺失"
	}
	realm, ok := RealmOf(jc.Provider)
	if !ok {
		return jobStatusFailed, "渠道不属于 WorkBuddy"
	}
	// 国际版专属通道：国内版零 HTTP 请求直接跳过（wb_accounts.py:615-616）。
	if realm != RealmIntl {
		return jobStatusSkipped, "该通道为国际版网页打卡"
	}
	if jc.Tok == nil {
		return jobStatusFailed, "渠道凭据缺失"
	}

	base, err := webBaseURL(realm)
	if err != nil || strings.TrimSpace(base) == "" {
		return jobStatusFailed, "构造网页通道端点失败"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := jc.httpClient()
	hdr := webConsoleHeaders(jc.Tok, realm)
	started := time.Now()

	// 步骤 1：建会话。注意此时只是排队（见文件头 v1.6.9 说明）。
	convID, failMsg := webCreateConversation(ctx, client, base, hdr)
	if failMsg != "" {
		return jobStatusFailed, failMsg
	}

	// 步骤 2：取沙箱 link/token。
	sess, failMsg := webLoadSession(ctx, client, base, hdr, convID)
	if failMsg != "" {
		return jobStatusFailed, withWebConversation(failMsg, convID)
	}

	// 步骤 3+4+5：接沙箱、发 JSON-RPC、读 SSE、轮询到 completed。
	timeout := webTurnTimeout()
	poll := func(pollCtx context.Context) (string, error) {
		return webPollConversationStatus(pollCtx, client, base, hdr, convID)
	}
	chunks, failMsg := webDriveTurn(ctx, client, sess, timeout, poll)
	if failMsg != "" {
		// 失败也把 conversation id 带上，便于用户/运维核对（id 不是秘密）。
		return jobStatusFailed, withWebConversation(failMsg, convID)
	}

	// 只有真的跑完才记 last conversation id（尽力而为，失败忽略）。
	if perr := persistWebConversation(jc, convID); perr != nil {
		webMetaWarnOnce.Do(func() {
			log.Printf("[workbuddy] web checkin meta persist failed: %v", perr)
		})
	}

	elapsed := time.Since(started)
	// 成功摘要只允许：状态字 + SSE 段数 + 耗时（绝不记输出内容）。
	return jobStatusOK, fmt.Sprintf("completed segments=%d elapsed=%s", chunks, elapsed.Round(time.Millisecond))
}

// webConsoleHeaders 构造 console API 的出站头。
//
// 对齐 wb_accounts.py:592-602（web_headers）：网页通道只需要
// Authorization: Bearer + X-User-Id，**不带** X-IDE-* / X-Product / X-Domain
// 等桌面端指纹头（那些会走 wb_identity.build_identity_headers，这里是另一套身分）。
func webConsoleHeaders(tok *oauth.TokenSet, realm Realm) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+tok.AccessToken)
	h.Set("X-User-Id", UIDFrom(tok))
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	origin := "https://" + identDomain(realm)
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/app")
	h.Set("User-Agent", webUserAgent)
	return h
}

// webCreateConversation 步骤 1：POST {base}/console/as/conversations/ 并解析会话 id。
// 返回 (conversationID, failMessage)；failMessage 非空表示失败。
func webCreateConversation(ctx context.Context, client *http.Client, base string, hdr http.Header) (string, string) {
	payload, err := json.Marshal(map[string]any{
		"prompt": webPromptText,
		"model":  webDefaultModel,
		// 网页端建会话固定带这两项（wb_accounts.py:617-623）。
		"conversationOrigin": webConversationOrigin,
		"plugins":            []map[string]string{{"name": webPluginName, "marketplace": webPluginMarketplace}},
	})
	if err != nil {
		return "", "构建创建会话请求失败"
	}
	target := webConsoleURL(base)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", "构建创建会话请求失败"
	}
	req.Header = hdr.Clone()

	resp, err := client.Do(req)
	if err != nil {
		return "", webTransportMessage(ctx, "创建会话请求")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJobBodyBytes))
	if err != nil {
		return "", "读取创建会话响应失败"
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", jobUpstreamFailMessage("创建会话", resp.StatusCode, raw)
	}

	convID, code, ok := parseWebConversation(raw)
	if !ok || !webCodeOK(code) || convID == "" {
		summary := upstreamErrSummary(raw)
		if summary == "" {
			summary = fmt.Sprintf("code=%v", code)
		}
		return "", "创建会话失败: " + truncateRunes(summary, maxProbeErrLen)
	}
	return convID, ""
}

// webSession 沙箱连接信息（wb_accounts.py:652-655）。
type webSession struct {
	link      string
	token     string
	sessionID string
	cwd       string
}

// webLoadSession 步骤 2：GET .../{id}/session → {link, token, sessionId, cwd}。
func webLoadSession(ctx context.Context, client *http.Client, base string, hdr http.Header, convID string) (webSession, string) {
	target := webConversationURL(base, convID) + "/session"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return webSession{}, "构建会话沙箱请求失败"
	}
	req.Header = hdr.Clone()

	resp, err := client.Do(req)
	if err != nil {
		return webSession{}, webTransportMessage(ctx, "查询会话沙箱")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJobBodyBytes))
	if err != nil {
		return webSession{}, "读取会话沙箱响应失败"
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return webSession{}, jobUpstreamFailMessage("查询会话沙箱", resp.StatusCode, raw)
	}

	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return webSession{}, "解析会话沙箱响应失败"
	}
	data, _ := dataObject(root)
	sess := webSession{
		link:  firstNonEmpty(toText(data["link"]), toText(data["endpoint"])),
		token: toText(data["token"]),
		// sessionId 缺省回落到 conversation id（wb_accounts.py:654）。
		sessionID: firstNonEmpty(toText(data["sessionId"]), toText(data["session_id"]), convID),
		cwd:       firstNonEmpty(toText(data["cwd"]), "/workspace"),
	}
	if sess.link == "" || sess.token == "" {
		return webSession{}, "沙箱未就绪（没有 link/token）"
	}
	return sess, ""
}

// webDriveTurn 步骤 3+4+5：把这一轮真正驱动到结束。
//
// 返回 (agent_message_chunk 段数, 空串)；失败时第二项为非空摘要。
// 只有 poll 返回 completed 才算成功 —— 见文件头 v1.6.9 说明。
func webDriveTurn(ctx context.Context, client *http.Client, sess webSession, timeout time.Duration, poll func(context.Context) (string, error)) (int64, string) {
	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ch := &acpChannel{client: client, link: sess.link, token: sess.token, userAgent: webUserAgent}
	if err := ch.open(turnCtx); err != nil {
		return 0, webTurnErrText(turnCtx, "连接沙箱失败", err)
	}
	// defer close：等待 SSE 读取协程退出并关闭响应体，保证不泄漏、不超出本函数。
	defer ch.close()

	// initialize：协议版本 1，客户端能力全关（打卡不需要文件系统/终端回调）。
	initParams := map[string]any{
		"protocolVersion": webProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}
	if err := ch.request(turnCtx, "initialize", initParams, 1); err != nil {
		return 0, webTurnErrText(turnCtx, "初始化会话失败", err)
	}

	loadParams := map[string]any{"sessionId": sess.sessionID, "cwd": sess.cwd, "mcpServers": []any{}}
	if err := ch.request(turnCtx, "session/load", loadParams, 2); err != nil {
		return 0, webTurnErrText(turnCtx, "加载会话失败", err)
	}

	promptParams := map[string]any{
		"sessionId": sess.sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": webPromptText}},
	}
	if err := ch.request(turnCtx, "session/prompt", promptParams, 3); err != nil {
		return 0, webTurnErrText(turnCtx, "提交打卡会话失败", err)
	}

	deadline := time.Now().Add(timeout)
	lastStatus := ""
	for {
		if err := turnCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return 0, fmt.Sprintf("会话在 %s 内没有跑完（状态=%s）", timeout, statusOrUnknown(lastStatus))
			}
			return 0, "任务已取消"
		}
		st, err := poll(turnCtx)
		if err == nil {
			lastStatus = st
			switch st {
			case "completed":
				return ch.chunks.Load(), ""
			case "failed", "error":
				// ★v1.6.9★：会话失败就直接失败，绝不因为「建过会话」而报 ok。
				return 0, "会话状态=" + st
			}
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Sprintf("会话在 %s 内没有跑完（状态=%s）", timeout, statusOrUnknown(lastStatus))
		}
		select {
		case <-turnCtx.Done():
		case <-time.After(webPollInterval):
		}
	}
}

// webPollConversationStatus GET .../{id}，取 data.status（wb_accounts.py:678-685）。
func webPollConversationStatus(ctx context.Context, client *http.Client, base string, hdr http.Header, convID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webConversationURL(base, convID), nil)
	if err != nil {
		return "", errors.New("构建状态查询请求失败")
	}
	req.Header = hdr.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJobBodyBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("状态查询返回 HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return "", errors.New("状态响应不可解析")
	}
	data, _ := dataObject(root)
	return toText(data["status"]), nil
}

// acpChannel ACP over streamable-HTTP：一条 SSE 收事件，POST 发 JSON-RPC。
// 对应 wb_webagent.py:38-160 的 AcpChannel。
type acpChannel struct {
	client    *http.Client
	link      string
	token     string
	userAgent string
	connID    string

	updates atomic.Int64
	chunks  atomic.Int64

	body       io.ReadCloser
	bodyOnce   sync.Once
	readerDone chan struct{}
	stopWatch  chan struct{}
	watchOnce  sync.Once
}

// open 建立 SSE 通道并取回 Acp-Connection-Id（wb_webagent.py:75-96）。
func (c *acpChannel) open(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.link, nil)
	if err != nil {
		return errors.New("构建 SSE 请求失败")
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		return webContextErr(ctx, "SSE 连接失败")
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return fmt.Errorf("SSE 通道返回 HTTP %d", resp.StatusCode)
	}
	connID := strings.TrimSpace(resp.Header.Get("Acp-Connection-Id"))
	if connID == "" {
		_ = resp.Body.Close()
		return errors.New("SSE 通道没有返回 Acp-Connection-Id")
	}

	c.connID = connID
	c.body = resp.Body
	c.readerDone = make(chan struct{})
	c.stopWatch = make(chan struct{})
	go c.watchCancel(ctx)
	go c.readLoop(resp.Body)
	return nil
}

// request 发一个 JSON-RPC 请求，正常返回 200/202（wb_webagent.py:128-151）。
func (c *acpChannel) request(ctx context.Context, method string, params any, id int) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("构建 %s 请求失败", method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.link, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("构建 %s 请求失败", method)
	}
	// 与 wb_webagent.py:136-140 完全一致的头集合与 Content-Type。
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Acp-Connection-Id", c.connID)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		return webContextErr(ctx, method+" 请求失败")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJobBodyBytes))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s 返回 HTTP %d", method, resp.StatusCode)
	}
	return nil
}

// watchCancel ctx 取消时关闭响应体，解除 readLoop 的阻塞读（不是唯一退出路径：
// close() 也会关，二者经 bodyOnce 幂等）。
func (c *acpChannel) watchCancel(ctx context.Context) {
	select {
	case <-ctx.Done():
		c.closeBody()
	case <-c.stopWatch:
	}
}

// readLoop 解析 SSE 流，统计 session/update 与 agent_message_chunk（wb_webagent.py:98-126）。
// 退出时 close(readerDone) 并调用测试钩子；由 close() 等待，保证协程不超出函数生命周期。
func (c *acpChannel) readLoop(body io.Reader) {
	defer close(c.readerDone)
	defer webSSEReaderExited()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxJobBodyBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var message map[string]any
		if json.Unmarshal([]byte(payload), &message) != nil {
			continue
		}
		if method, _ := message["method"].(string); method != "session/update" {
			continue
		}
		c.updates.Add(1)
		params, _ := message["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		if kind, _ := update["sessionUpdate"].(string); kind == "agent_message_chunk" {
			c.chunks.Add(1)
		}
	}
}

// close 停止监听、关闭响应体并等待读取协程退出（幂等，可重复调用）。
func (c *acpChannel) close() {
	if c == nil {
		return
	}
	if c.stopWatch != nil {
		c.watchOnce.Do(func() { close(c.stopWatch) })
	}
	c.closeBody()
	if c.readerDone != nil {
		<-c.readerDone
	}
}

// closeBody 幂等关闭响应体。
func (c *acpChannel) closeBody() {
	if c == nil || c.body == nil {
		return
	}
	c.bodyOnce.Do(func() { _ = c.body.Close() })
}

// ---- 元数据持久化（尽力而为） ----

// webMetaStore 网页打卡持久化 last-conversation-id 所需的最小能力（*op.Op 满足）。
type webMetaStore interface {
	UpdateWorkbuddyMeta(userID, providerID int64, key string, value any, now time.Time) error
}

// webMetaSinkFor 返回元数据写入实现：测试注入的假 DB 优先，否则用 Op。
// 与 Runtime.jobStore() 同款优先级，便于单测零 DB 断言 Meta。
func webMetaSinkFor(jc *JobContext) webMetaStore {
	if jc == nil {
		return nil
	}
	if jc.Runtime != nil && jc.Runtime.dbs != nil {
		if s, ok := jc.Runtime.dbs.(webMetaStore); ok {
			return s
		}
	}
	if jc.Op == nil {
		return nil
	}
	return jc.Op
}

// persistWebConversation 把最后一条跑完的会话 id 写进 Meta（失败由调用方忽略）。
func persistWebConversation(jc *JobContext, convID string) error {
	convID = strings.TrimSpace(convID)
	if jc == nil || jc.Provider == nil || convID == "" {
		return nil
	}
	sink := webMetaSinkFor(jc)
	if sink == nil {
		return nil
	}
	now := jc.Now
	if now.IsZero() {
		now = time.Now()
	}
	return sink.UpdateWorkbuddyMeta(jc.Provider.UserID, jc.Provider.ID, webMetaKeyLastConversation, convID, now)
}

// ---- 小工具 ----

// webConsoleURL 归一化基址（去尾斜杠）并补 WebConversationsPath 前缀。
func webConsoleURL(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + WebConversationsPath
}

// webConversationURL 单条会话 URL（path escape 与 wb_accounts.py:673 的 quote 对齐）。
func webConversationURL(base, convID string) string {
	return webConsoleURL(base) + url.PathEscape(convID)
}

// parseWebConversation 解析 {code, data:{id}}；ok 仅表示 JSON 对象可解析。
func parseWebConversation(raw []byte) (id string, code any, ok bool) {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return "", nil, false
	}
	code = root["code"]
	if data, ok := dataObject(root); ok {
		id = toText(data["id"])
	}
	if id == "" {
		id = toText(root["id"])
	}
	return id, code, true
}

// webCodeOK 上游业务码为 0 / 缺失视为成功（wb_accounts.py:642）。
func webCodeOK(code any) bool {
	if code == nil {
		return true
	}
	if f, ok := toFloat(code); ok {
		return f == 0
	}
	s := toText(code)
	return s == "" || s == "0"
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// statusOrUnknown 状态为空时显示「未知」（wb_webagent.py:217）。
func statusOrUnknown(status string) string {
	if strings.TrimSpace(status) == "" {
		return "未知"
	}
	return status
}

// withWebConversation 失败摘要追加 conversation id（非秘密，便于核对）。
func withWebConversation(msg, convID string) string {
	if strings.TrimSpace(convID) == "" {
		return msg
	}
	return fmt.Sprintf("%s（conversation=%s）", msg, convID)
}

// webTurnTimeout 单轮超时：env 优先，支持 Go duration 或整数秒，非法/非正回落 120s。
func webTurnTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(webTurnTimeoutEnv))
	if raw == "" {
		return webDefaultTurnTimeout
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return webDefaultTurnTimeout
}

// webContextErr ctx 已取消/超时时返回 ctx 错误（供上层统一翻译），否则返回固定文案。
func webContextErr(ctx context.Context, fallback string) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New(fallback)
}

// webTransportMessage 传输层错误摘要：超时/取消用固定文案，其余不带 URL（默认客户端
// 错误串含请求 URL，可能带沙箱签名参数，故只报固定文案）。
func webTransportMessage(ctx context.Context, prefix string) string {
	if ctx != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "会话超时"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "任务已取消"
		}
	}
	return prefix + "失败（传输层错误）"
}

// webTurnErrText 把 acpChannel 的错误翻成可入 Message 的摘要（无正文、无 token）。
func webTurnErrText(ctx context.Context, prefix string, err error) string {
	if ctx != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "会话超时"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "任务已取消"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "会话超时"
	}
	if errors.Is(err, context.Canceled) {
		return "任务已取消"
	}
	if err == nil {
		return prefix
	}
	return prefix + ": " + truncateRunes(err.Error(), webErrTextLen)
}
