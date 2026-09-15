package handlers

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/resp"
)

// AdminUsageHandler 全平台用量看板：聚合 + TOP 排行 + CSV 导出。
type AdminUsageHandler struct {
	Op *op.Op
}

// GET /api/admin/usage?by=user|model|day&days=N 或 ?from=&to=
func (h *AdminUsageHandler) Get(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "day"
	}
	if by != "user" && by != "model" && by != "day" {
		resp.BadRequest(w, "by must be user, model or day")
		return
	}
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	rows, err := h.Op.AggregatePlatformUsage(by, days, from, to)
	if err != nil {
		resp.Internal(w, "aggregate usage failed")
		return
	}
	if rows == nil {
		rows = []op.PlatformUsageAggRow{}
	}
	resp.Success(w, rows)
}

// GET /api/admin/usage/top?days=N&limit=10 或 ?from=&to=
func (h *AdminUsageHandler) Top(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	rows, err := h.Op.TopUsersByTokens(days, limit, from, to)
	if err != nil {
		resp.Internal(w, "top users failed")
		return
	}
	if rows == nil {
		rows = []op.TopUserRow{}
	}
	resp.Success(w, rows)
}

// GET /api/admin/usage/hourly?days=N 或 ?from=&to= —— 星期×小时聚合（分时活跃热力图用）。
func (h *AdminUsageHandler) Hourly(w http.ResponseWriter, r *http.Request) {
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	rows, err := h.Op.AggregatePlatformUsageHourly(days, from, to)
	if err != nil {
		resp.Internal(w, "aggregate usage hourly failed")
		return
	}
	if rows == nil {
		rows = []op.UsageHourRow{}
	}
	resp.Success(w, rows)
}

// GET /api/admin/usage/export?by=user|model|day&days=N 或 ?from=&to=  → CSV
// 表头按 by 维度动态生成（day 维度不含恒空的 user/model 列），并补充 total_tokens / cost 列，
// 与页面明细表口径一致；避免导出一堆空列让数据“看起来变少”。
func (h *AdminUsageHandler) Export(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "day"
	}
	if by != "user" && by != "model" && by != "day" {
		resp.BadRequest(w, "by must be user, model or day")
		return
	}
	days, from, to, err := parseUsageRange(r)
	if err != nil {
		resp.BadRequest(w, "invalid from/to: "+err.Error())
		return
	}
	rows, err := h.Op.AggregatePlatformUsage(by, days, from, to)
	if err != nil {
		resp.Internal(w, "aggregate usage failed")
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=usage_%s_%s.csv", by, time.Now().Format("20060102_150405")))
	cw := csv.NewWriter(w)
	for _, rec := range buildCSVRows(by, rows) {
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// buildCSVRows 组装 CSV 表头与数据行（纯函数，便于单测）：
// 维度列只保留当前 by 有意义的（day 维度不含恒空的 user/model 列），
// 数值列统一补充 total_tokens 与 cost，与页面明细表口径一致。
func buildCSVRows(by string, rows []op.PlatformUsageAggRow) [][]string {
	var header []string
	switch by {
	case "user":
		header = []string{"day", "user_id", "email", "requests", "failed_requests", "prompt_tokens", "completion_tokens", "total_tokens", "cost", "avg_latency_ms"}
	case "model":
		header = []string{"model", "requests", "failed_requests", "prompt_tokens", "completion_tokens", "total_tokens", "cost", "avg_latency_ms"}
	default:
		header = []string{"day", "requests", "failed_requests", "prompt_tokens", "completion_tokens", "total_tokens", "cost", "avg_latency_ms"}
	}
	out := [][]string{header}
	for _, row := range rows {
		var rec []string
		switch by {
		case "user":
			rec = append(rec, row.Day, csvSafe(strconv.FormatInt(row.UserID, 10)), csvSafe(row.Email))
		case "model":
			rec = append(rec, csvSafe(row.Model))
		default:
			rec = append(rec, row.Day)
		}
		rec = append(rec,
			strconv.FormatInt(row.Requests, 10),
			strconv.FormatInt(row.FailedRequests, 10),
			strconv.FormatInt(row.PromptTokens, 10),
			strconv.FormatInt(row.CompletionTokens, 10),
			strconv.FormatInt(row.PromptTokens+row.CompletionTokens, 10),
			csvFloat(row.Cost),
			fmt.Sprintf("%.1f", row.AvgLatencyMs),
		)
		out = append(out, rec)
	}
	return out
}

// csvSafe 防 CSV 公式注入：以 = + - @ 开头的单元格前插单引号（Excel/Sheets 视为文本）。
func csvSafe(s string) string {
	if len(s) > 0 && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@') {
		return "'" + s
	}
	return s
}

// csvFloat 费用列：金额量级小（单行可能 $0.0001 级），用最短精确表示，避免精度丢失。
func csvFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
