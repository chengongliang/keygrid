package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/quota"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"

	"github.com/go-chi/chi/v5"
)

// quota.go OAuth 渠道额度查询（openai codex /wham/usage）。
// List 读快照（后台任务 / relay 被动观察负责更新）；Refresh 手动触发一次上游查询。

type QuotaHandler struct {
	Op     *op.Op
	Syncer *quota.Syncer
}

// List GET /api/quota —— 本人全部渠道的额度快照（读库，不打上游）。
func (h *QuotaHandler) List(w http.ResponseWriter, r *http.Request) {
	snaps, err := h.Op.ListQuotaSnapshots(middleware.UserID(r.Context()))
	if err != nil {
		resp.Internal(w, "list quota failed")
		return
	}
	if snaps == nil {
		snaps = []model.QuotaSnapshot{}
	}
	resp.Success(w, snaps)
}

// Refresh POST /api/providers/{id}/quota/refresh —— 手动触发一次上游用量查询，
// 返回最新快照。30s 软限频：窗口内重复请求直接返回缓存快照（cached=true）。
func (h *QuotaHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "invalid id")
		return
	}
	// 归属校验（user_id 硬隔离）：他人渠道一律 404
	p, err := h.Op.GetProvider(userID, id)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}
	if !quota.SupportsQuota(p) {
		resp.BadRequest(w, "quota query is only available for openai oauth channels")
		return
	}
	snap, cached, err := h.Syncer.RefreshProvider(r.Context(), p)
	if err != nil {
		if cached {
			// 限频窗口内：上游查询被跳过，无历史快照时返回空（前端保持现状）
			if errors.Is(err, op.ErrNotFound) {
				resp.Success(w, nil)
				return
			}
			// 快照读取失败但探测未发生：按内部错误处理
			resp.Internal(w, "read quota snapshot failed")
			return
		}
		// 真实探测失败：502 透出上游错误（快照里也留了 error 标记）
		resp.BadGateway(w, err.Error())
		return
	}
	resp.Success(w, snap)
}
