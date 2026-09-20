package relay

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

// reqerr.go 失败请求诊断记录：失败分类 + 上游错误摘要提取 + 异步批量落库。
//
// 为什么单开一张表（而不是给 usage_logs 加列）：
//   - usage_logs 是统计口径（每次请求一行、参与聚合与 CSV 导出），不该被诊断字段污染；
//   - 诊断数据可过期，单独一张表能整体清理（默认 7 天，见 task 后台任务）；
//   - 渠道故障期写入量大，独立队列可在压力下直接丢弃诊断，不阻塞记账。
//
// 隐私边界（AGENTS 硬性规则 4）：只落「元数据 + 上游错误摘要」。
// 上游非 2xx 正文可能回显 prompt 片段（见 UpstreamError.Detail 注释），所以
// 绝不落 body 原文：只从 JSON 里提取结构化字段 error.message / error.code /
// error.type，解析不出结构化字段时退化为「content-type + 字节数」描述。

// 失败分类（落 request_errors.kind，前端按此归类展示）
const (
	errKindNoChannel     = "no_channel"     // 没有任何可用候选渠道（模型不支持 / 渠道被禁用 / key 白名单过滤）
	errKindCircuitOpen   = "circuit_open"   // 渠道熔断中，本次未发起上游调用
	errKindCredential    = "credential"     // 凭据缺失 / 解密失败 / 已吊销
	errKindTransform     = "transform"      // 入口协议 → 上游协议转换失败
	errKindProxy         = "proxy"          // 渠道要求走平台代理但代理未配置或不可用
	errKindUpstream4xx   = "upstream_4xx"   // 上游 4xx：请求/凭据/权限问题（不计熔断）
	errKindUpstream429   = "upstream_429"   // 上游限流
	errKindUpstream5xx   = "upstream_5xx"   // 上游服务端错误
	errKindTransport     = "transport"      // 连接 / DNS / TLS / 超时等传输层错误
	errKindStreamAborted = "stream_aborted" // 流式响应已开始后上游中断（客户端看到截断）
)

// maxErrMessageLen 落库的上游错误摘要长度上限（rune）。
const maxErrMessageLen = 512

// classifyUpstreamStatus 按 HTTP 状态码给上游失败分类（与熔断口径保持一致：
// 429/5xx 算渠道故障，4xx 算请求问题）。
func classifyUpstreamStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return errKindUpstream429
	case status >= 500:
		return errKindUpstream5xx
	case status >= 400:
		return errKindUpstream4xx
	}
	return errKindTransport
}

// sanitizeErrMessage 把上游响应正文压成可落库的错误摘要。
//
// 只取结构化字段，绝不落正文原文：
//   - JSON body：提取 error.message（OpenAI/Anthropic/Messages 风格）、
//     error.type / error.code，拼成「message (type=…, code=…)」；
//   - 非 JSON（HTML 错误页、CDN 拦截页…）：只描述 content-type 与字节数；
//   - 解析出内容为空：退回「status/长度」级别的描述。
//
// 换行归一化为空格（错误消息要进表格单元格），并截断到 maxErrMessageLen。
func sanitizeErrMessage(body, contentType string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	var probe struct {
		Error struct {
			Message any    `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Status  string `json:"status"`
		} `json:"error"`
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal([]byte(body), &probe) == nil {
		msg := anyString(probe.Error.Message)
		if msg == "" {
			msg = probe.Message
		}
		if msg == "" {
			// 有些上游用 error.status / error.type 表达原因（如 anthropic overloaded_error）
			msg = firstNonEmpty(probe.Error.Status, probe.Error.Type, probe.Type)
		}
		if msg != "" {
			var extras []string
			if t := firstNonEmpty(probe.Error.Type, probe.Type); t != "" {
				extras = append(extras, "type="+t)
			}
			if c := anyString(probe.Error.Code); c != "" {
				extras = append(extras, "code="+c)
			}
			if len(extras) > 0 {
				msg += " (" + strings.Join(extras, ", ") + ")"
			}
			return truncateErrMessage(normalizeErrWhitespace(msg))
		}
		return "" // JSON 但没有可识别的错误字段：不留正文（可能含请求回显）
	}
	// 非 JSON：只记形状不记内容（HTML 拦截页/网关页常见，正文可能非常大且含请求回显）
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		ct = "unknown"
	}
	return fmt.Sprintf("non-JSON upstream body (%s, %d bytes)", ct, len(body))
}

// normalizeErrWhitespace 把换行/连续空白压成单个空格（错误摘要要进表格单元格）。
func normalizeErrWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateErrMessage 按 rune 截断（不切断多字节字符），超出时追加省略号。
func truncateErrMessage(s string) string {
	r := []rune(s)
	if len(r) <= maxErrMessageLen {
		return s
	}
	return string(r[:maxErrMessageLen]) + "…"
}

// anyString 把 JSON 里可能是 string/number/bool 的字段转成字符串。
func anyString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		// 整数码值（如 429）不要输出 429.000000
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// ErrorStore 失败诊断落库接口（*op.Op 满足）。
type ErrorStore interface {
	InsertRequestErrors(rows []*model.RequestError) error
}

// maxErrorsPerProviderPerMinute 单渠道每分钟落库上限。
//
// 上游长时间故障（尤其熔断反复打开）时失败会以请求频率堆积，而诊断只需要
// 样本与分类分布，不需要「每次失败一行」—— 超限直接丢弃，防止把表写爆。
const maxErrorsPerProviderPerMinute = 30

// ErrorWriter 失败诊断异步批量写入器（与 UsageWriter 同构）。
//
// 转发路径只投递不落库；队列满时丢弃并打一次日志 —— 渠道故障风暴期间宁可
// 丢诊断，也不能让诊断写入拖慢转发。
type ErrorWriter struct {
	ch     chan *model.RequestError
	store  ErrorStore
	batch  int
	period time.Duration

	// 采样窗口（按渠道计数，每分钟一翻转）
	mu       sync.Mutex
	winStart time.Time
	winCount map[int64]int
	// now 可注入时钟（测试用）
	now func() time.Time

	dropOnce sync.Once
	stopOnce sync.Once
	done     chan struct{}
}

func NewErrorWriter(store ErrorStore, batchSize int, flushPeriod time.Duration) *ErrorWriter {
	if batchSize <= 0 {
		batchSize = 50
	}
	if flushPeriod <= 0 {
		flushPeriod = 2 * time.Second
	}
	ew := &ErrorWriter{
		ch:       make(chan *model.RequestError, 2048),
		store:    store,
		batch:    batchSize,
		period:   flushPeriod,
		winCount: map[int64]int{},
		now:      time.Now,
		done:     make(chan struct{}),
	}
	go ew.run()
	return ew
}

// allow 按渠道限频采样：同一渠道每分钟最多落库 maxErrorsPerProviderPerMinute 条。
func (ew *ErrorWriter) allow(providerID int64) bool {
	now := ew.now()
	ew.mu.Lock()
	defer ew.mu.Unlock()
	if ew.winStart.IsZero() || now.Sub(ew.winStart) >= time.Minute {
		ew.winStart = now
		ew.winCount = map[int64]int{}
	}
	if ew.winCount[providerID] >= maxErrorsPerProviderPerMinute {
		return false
	}
	ew.winCount[providerID]++
	return true
}

// Record 异步投递一条失败诊断（永不阻塞转发路径；命中采样限频则丢弃）。
func (ew *ErrorWriter) Record(row *model.RequestError) {
	if ew == nil || row == nil {
		return
	}
	if !ew.allow(row.ProviderID) {
		return
	}
	select {
	case ew.ch <- row:
	default:
		ew.dropOnce.Do(func() {
			log.Printf("[relay] request error queue full, dropping diagnostics")
		})
	}
}

func (ew *ErrorWriter) run() {
	defer close(ew.done)
	ticker := time.NewTicker(ew.period)
	defer ticker.Stop()
	batch := make([]*model.RequestError, 0, ew.batch)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := ew.store.InsertRequestErrors(batch); err != nil {
			log.Printf("[relay] request error flush failed (%d rows): %v", len(batch), err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case row, ok := <-ew.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, row)
			if len(batch) >= ew.batch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Stop 落库剩余记录后退出（进程关闭时调用）。
func (ew *ErrorWriter) Stop() {
	if ew == nil {
		return
	}
	ew.stopOnce.Do(func() {
		close(ew.ch)
		<-ew.done
	})
}
