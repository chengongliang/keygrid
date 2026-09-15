package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// UsageHandler GET /api/usage?days=N —— 按日/模型聚合本人用量。
// 支持 ?key_id=（平台签发的 Key）与 ?model= 筛选。
type UsageHandler struct {
	Op *op.Op
}

// parseUsageRange 解析 ?days=N 或 ?from=&to=（from/to 至少一个生效则优先于 days）。
func parseUsageRange(r *http.Request) (days int, from, to *time.Time, err error) {
	if d := r.URL.Query().Get("days"); d != "" {
		if n, e := strconv.Atoi(d); e == nil && n > 0 {
			days = n
		}
	}
	if v := r.URL.Query().Get("from"); v != "" {
		t, e := op.ParseUsageTime(v)
		if e != nil {
			return 0, nil, nil, e
		}
		from = &t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, e := op.ParseUsageTime(v)
		if e != nil {
			return 0, nil, nil, e
		}
		to = &t
	}
	return days, from, to, nil
}

// parseUsageFilter 解析 ?key_id=&provider_id=&model= 筛选参数（缺省 = 不过滤）。
func parseUsageFilter(r *http.Request) (op.UsageFilter, error) {
	f := op.UsageFilter{}
	if v := r.URL.Query().Get("key_id"); v != "" {
		id, e := strconv.ParseInt(v, 10, 64)
		if e != nil || id <= 0 {
			return f, strconv.ErrSyntax
		}
		f.ApiKeyID = id
	}
	if v := r.URL.Query().Get("provider_id"); v != "" {
		id, e := strconv.ParseInt(v, 10, 64)
		if e != nil || id <= 0 {
			return f, strconv.ErrSyntax
		}
		f.ProviderID = id
	}
	f.Model = strings.TrimSpace(r.URL.Query().Get("model"))
	return f, nil
}

func (h *UsageHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	f, err := parseUsageFilter(r)
	if err != nil {
		resp.BadRequest(w, "invalid key_id")
		return
	}
	rows, err := h.Op.AggregateUsage(userID, days, from, to, f)
	if err != nil {
		resp.Internal(w, "aggregate usage failed")
		return
	}
	if rows == nil {
		rows = []op.UsageAggRow{}
	}
	resp.Success(w, rows)
}

// UsageSummary GET /api/usage/summary —— 本人费用汇总（累计总费用 / 近 30 天 / tokens / 请求数）。
// 只展示不拦截 —— 额度硬限只在 key 级（用户无总额概念）。
func (h *UsageHandler) Summary(w http.ResponseWriter, r *http.Request) {
	row, err := h.Op.UserCostSummary(middleware.UserID(r.Context()))
	if err != nil {
		resp.Internal(w, "usage summary failed")
		return
	}
	resp.Success(w, row)
}

// UsageHourly GET /api/usage/hourly?days=N —— 星期×小时聚合（分时活跃热力图用）。
func (h *UsageHandler) UsageHourly(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	f, err := parseUsageFilter(r)
	if err != nil {
		resp.BadRequest(w, "invalid key_id")
		return
	}
	rows, err := h.Op.AggregateUsageHourly(userID, days, from, to, f)
	if err != nil {
		resp.Internal(w, "aggregate usage hourly failed")
		return
	}
	if rows == nil {
		rows = []op.UsageHourRow{}
	}
	resp.Success(w, rows)
}

// ByKey GET /api/usage/by-key —— 按平台签发的 Key 汇总用量（Key 用量表）。
// 不接受 key_id 过滤（否则只剩一行）；可带 model 过滤，看"某模型在各 Key 上的用量"。
func (h *UsageHandler) ByKey(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	rows, err := h.Op.AggregateUsageByKey(userID, days, from, to, model)
	if err != nil {
		resp.Internal(w, "aggregate usage by key failed")
		return
	}
	if rows == nil {
		rows = []op.UsageByKeyRow{}
	}
	resp.Success(w, rows)
}

// ByProvider GET /api/usage/by-provider —— 按渠道（供应商）汇总本人用量（渠道用量表）。
// 渠道本就是用户自助配置的，统计仅元数据聚合且与路由同口径（强制 user_id 隔离）。
// 不接受 provider_id 过滤（否则只剩一行）；可带 key_id/model 过滤，
// 看“某 Key/模型在哪些渠道上的用量”。
func (h *UsageHandler) ByProvider(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	f, err := parseUsageFilter(r)
	if err != nil {
		resp.BadRequest(w, "invalid key_id")
		return
	}
	rows, err := h.Op.AggregateUsageByProvider(userID, days, from, to, f)
	if err != nil {
		resp.Internal(w, "aggregate usage by provider failed")
		return
	}
	if rows == nil {
		rows = []op.UsageByProviderRow{}
	}
	resp.Success(w, rows)
}

// ByModel GET /api/usage/by-model —— 按模型汇总用量（模型用量表 + 模型筛选下拉）。
// 不接受 model 过滤（否则只剩一行）；可带 key_id 过滤，看"某 Key 用了哪些模型、各多少"。
func (h *UsageHandler) ByModel(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	f, err := parseUsageFilter(r)
	if err != nil {
		resp.BadRequest(w, "invalid key_id")
		return
	}
	rows, err := h.Op.AggregateUsageByModel(userID, days, from, to, f.ApiKeyID)
	if err != nil {
		resp.Internal(w, "aggregate usage by model failed")
		return
	}
	if rows == nil {
		rows = []op.UsageByModelRow{}
	}
	resp.Success(w, rows)
}
