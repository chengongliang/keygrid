package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
	"github.com/chengongliang/keygrid/internal/workbuddy"
)

// workbuddy.go WorkBuddy / CodeBuddy 渠道的用户端 API
// （docs/workbuddy-integration-design.md §2.5）。
//
// 全部路由挂在 SessionAuth 之后；渠道与运行态查询一律带当前用户 user_id。
// 响应形状是前端契约（web/src/lib/api.ts 的 Workbuddy* 类型），改动需同步前端。

// workbuddyJobKeys 任务键全集，与前端 WorkbuddyJobKey 一一对应。
//
// 顺序即面板顺序：先低风险的运行态维护（签到/积分/兑换），再高风险的行为模拟。
var workbuddyJobKeys = []string{
	"checkin", "credits", "daily_chat",
	"growth", "night", "cat", "daily_chat_web",
}

// WorkbuddyHandler 用户端 WorkBuddy 面板 handler。
type WorkbuddyHandler struct {
	Op *op.Op
	// Runtime 运行态（准入/冷却/积分探测）；nil = 未启用
	Runtime *workbuddy.Runtime
	// Client 探测用 HTTP 客户端（上游域名是编译期常量，不经用户输入）
	Client *http.Client
}

// wbSettingsDTO 设置 DTO：jobs_enabled 补齐所有键，前端无需处理缺省。
type wbSettingsDTO struct {
	Enabled         bool            `json:"enabled"`
	ReserveCredits  float64         `json:"reserve_credits"`
	DailyTokenLimit int64           `json:"daily_token_limit"`
	Product         string          `json:"product"`
	ScrubMode       string          `json:"scrub_mode"`
	Fingerprint     bool            `json:"fingerprint"`
	JobsEnabled     map[string]bool `json:"jobs_enabled"`
}

type wbChannelDTO struct {
	ProviderID int64             `json:"provider_id"`
	Name       string            `json:"name"`
	Realm      string            `json:"realm"`
	Account    string            `json:"account"`
	Nickname   string            `json:"nickname"`
	Credits    float64           `json:"credits"`
	CreditsAt  string            `json:"credits_at,omitempty"`
	DailyToken int64             `json:"daily_tokens"`
	TokensDate string            `json:"tokens_date,omitempty"`
	Parked     string            `json:"parked_reason,omitempty"`
	Cooldowns  map[string]string `json:"cooldowns,omitempty"`
	LastOKAt   string            `json:"last_ok_at,omitempty"`
}

type wbRunDTO struct {
	JobKey     string `json:"job_key"`
	ProviderID int64  `json:"provider_id,omitempty"`
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type wbOverviewDTO struct {
	Settings wbSettingsDTO `json:"settings"`
	Platform struct {
		JobsAllowed bool `json:"jobs_allowed"`
	} `json:"platform"`
	Channels []wbChannelDTO `json:"channels"`
	Runs     []wbRunDTO     `json:"runs"`
}

// Overview GET /api/workbuddy/overview —— 面板一次拉全（设置 + 运行态 + 日志）。
func (h *WorkbuddyHandler) Overview(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())

	setting, err := h.Op.GetWorkbuddySetting(userID)
	if err != nil {
		resp.Internal(w, "读取设置失败")
		return
	}
	states, err := h.Op.ListWorkbuddyChannelStates(userID)
	if err != nil {
		resp.Internal(w, "读取渠道运行态失败")
		return
	}
	providers, err := h.Op.ListProviders(userID)
	if err != nil {
		resp.Internal(w, "读取渠道失败")
		return
	}
	runs, err := h.Op.ListWorkbuddyJobRuns(userID, 50)
	if err != nil {
		resp.Internal(w, "读取执行日志失败")
		return
	}

	out := wbOverviewDTO{Settings: toWbSettingsDTO(setting)}
	// 自动化任务总闸：平台设置（管理员一键全停）优先于用户配置；
	// 总闸关闭时前端把任务开关与"立即执行"全部置灰。
	out.Platform.JobsAllowed = h.Runtime != nil && h.Runtime.JobsAllowed()

	// 渠道运行态与渠道表按 provider_id 对齐；只展示仍属于当前用户的
	// WorkBuddy/CodeBuddy 渠道（运行态可能残留已删渠道的孤儿行）。
	statesByID := map[int64]model.WorkbuddyChannelState{}
	for _, s := range states {
		statesByID[s.ProviderID] = s
	}
	out.Channels = make([]wbChannelDTO, 0, len(statesByID))
	for _, p := range providers {
		if !workbuddy.IsWorkBuddy(&p) {
			continue
		}
		s := statesByID[p.ID]
		realm, _ := workbuddy.RealmOf(&p)
		dto := wbChannelDTO{
			ProviderID: p.ID,
			Name:       p.Name,
			Realm:      string(realm),
			Account:    s.AccountUID,
			Nickname:   s.Nickname,
			Credits:    s.Credits,
			DailyToken: s.DailyTokens,
			TokensDate: s.TokensDate,
			Parked:     s.ParkedReason,
		}
		if !s.CreditsAt.IsZero() {
			dto.CreditsAt = s.CreditsAt.UTC().Format(time.RFC3339)
		}
		if s.LastOKAt != nil && !s.LastOKAt.IsZero() {
			dto.LastOKAt = s.LastOKAt.UTC().Format(time.RFC3339)
		}
		if len(s.Cooldowns) > 0 {
			cd := map[string]string{}
			for k, v := range s.Cooldowns {
				cd[k] = v.UTC().Format(time.RFC3339)
			}
			dto.Cooldowns = cd
		}
		out.Channels = append(out.Channels, dto)
	}

	out.Runs = make([]wbRunDTO, 0, len(runs))
	for _, run := range runs {
		d := wbRunDTO{
			JobKey:    run.JobKey,
			Trigger:   run.Trigger,
			Status:    run.Status,
			Message:   run.Message,
			StartedAt: run.StartedAt.UTC().Format(time.RFC3339),
		}
		if run.ProviderID != nil {
			d.ProviderID = *run.ProviderID
		}
		if run.FinishedAt != nil && !run.FinishedAt.IsZero() {
			d.FinishedAt = run.FinishedAt.UTC().Format(time.RFC3339)
		}
		out.Runs = append(out.Runs, d)
	}
	resp.Success(w, out)
}

// toWbSettingsDTO 落库设置 → DTO（未知 product/scrub_mode 归一化成默认值，
// jobs_enabled 缺失的键补 false）。
func toWbSettingsDTO(s *model.WorkbuddySetting) wbSettingsDTO {
	dto := wbSettingsDTO{
		Enabled:         s.Enabled,
		ReserveCredits:  s.ReserveCredits,
		DailyTokenLimit: s.DailyTokenLimit,
		Product:         s.Product,
		ScrubMode:       s.ScrubMode,
		Fingerprint:     s.Fingerprint,
		JobsEnabled:     map[string]bool{},
	}
	if !validWbProduct(dto.Product) {
		dto.Product = string(workbuddy.ProductWorkbuddy)
	}
	if !validWbScrub(dto.ScrubMode) {
		dto.ScrubMode = workbuddy.ScrubOff
	}
	for _, k := range workbuddyJobKeys {
		dto.JobsEnabled[k] = s.JobsEnabled[k]
	}
	return dto
}

// wbSettingsPatch PATCH 入参：指针即"本次是否修改"（缺省字段不动）。
type wbSettingsPatch struct {
	Enabled         *bool            `json:"enabled"`
	ReserveCredits  *float64         `json:"reserve_credits"`
	DailyTokenLimit *int64           `json:"daily_token_limit"`
	Product         *string          `json:"product"`
	ScrubMode       *string          `json:"scrub_mode"`
	Fingerprint     *bool            `json:"fingerprint"`
	JobsEnabled     *map[string]bool `json:"jobs_enabled"`
}

// UpdateSettings PATCH /api/workbuddy/settings —— 局部更新（未传字段保持原值）。
func (h *WorkbuddyHandler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	var patch wbSettingsPatch
	if err := resp.Decode(r, &patch); err != nil {
		resp.BadRequest(w, "请求体格式错误")
		return
	}
	cur, err := h.Op.GetWorkbuddySetting(userID)
	if err != nil {
		resp.Internal(w, "读取设置失败")
		return
	}
	if patch.ReserveCredits != nil {
		if *patch.ReserveCredits < 0 {
			resp.BadRequest(w, "保留积分不能为负数")
			return
		}
		cur.ReserveCredits = *patch.ReserveCredits
	}
	if patch.DailyTokenLimit != nil {
		if *patch.DailyTokenLimit < 0 {
			resp.BadRequest(w, "每日 token 上限不能为负数")
			return
		}
		cur.DailyTokenLimit = *patch.DailyTokenLimit
	}
	if patch.Product != nil {
		if !validWbProduct(*patch.Product) {
			resp.BadRequest(w, "出站身分取值不合法")
			return
		}
		cur.Product = *patch.Product
	}
	if patch.ScrubMode != nil {
		if !validWbScrub(*patch.ScrubMode) {
			resp.BadRequest(w, "脱敏档位取值不合法")
			return
		}
		cur.ScrubMode = *patch.ScrubMode
	}
	if patch.Fingerprint != nil {
		cur.Fingerprint = *patch.Fingerprint
	}
	if patch.Enabled != nil {
		cur.Enabled = *patch.Enabled
		// 总开关关闭时不动任务开关：用户下次打开应恢复原选择。
	}
	if patch.JobsEnabled != nil {
		if cur.JobsEnabled == nil {
			cur.JobsEnabled = map[string]bool{}
		}
		for k, v := range *patch.JobsEnabled {
			if !isWbJobKey(k) {
				resp.BadRequest(w, "未知任务键: "+k)
				return
			}
			cur.JobsEnabled[k] = v
		}
	}
	cur.UserID = userID
	if err := h.Op.UpsertWorkbuddySetting(cur); err != nil {
		resp.Internal(w, "保存设置失败")
		return
	}
	// 用户重新打开总开关时清掉 disabled 停用标记（其他停用原因由判定逻辑自行恢复）
	if cur.Enabled {
		if states, err := h.Op.ListWorkbuddyChannelStates(userID); err == nil {
			for _, s := range states {
				if s.ParkedReason == "disabled" {
					_ = h.Op.SetWorkbuddyParked(userID, s.ProviderID, "", time.Now())
				}
			}
		}
	}
	resp.Success(w, map[string]bool{"ok": true})
}

// RefreshCredits POST /api/workbuddy/channels/{id}/credits —— 主动查询积分余额。
func (h *WorkbuddyHandler) RefreshCredits(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		resp.BadRequest(w, "渠道 ID 不合法")
		return
	}
	p, err := h.Op.GetProvider(userID, id)
	if err != nil {
		resp.NotFound(w, "渠道不存在")
		return
	}
	if !workbuddy.IsWorkBuddy(p) {
		resp.BadRequest(w, "该渠道不是 WorkBuddy/CodeBuddy 渠道")
		return
	}
	if h.Runtime == nil {
		resp.Internal(w, "运行态未初始化")
		return
	}
	tok := h.tokenSet(p)
	if tok == nil || tok.AccessToken == "" {
		resp.BadRequest(w, "渠道凭据不可用，请重新授权")
		return
	}
	credits, err := h.Runtime.ProbeCredits(r.Context(), h.httpClient(), p, tok)
	if err != nil {
		// 上游错误摘要已由 ProbeCredits 裁剪（无正文、无 token），可安全回传
		resp.BadGateway(w, "查询积分失败: "+err.Error())
		return
	}
	resp.Success(w, map[string]any{
		"credits":    credits,
		"credits_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// RunJob POST /api/workbuddy/jobs/{job}/run —— 手动触发任务。
//
// 与调度路径共用幂等键（防连点）：同一天同一渠道同一任务只会真正执行一次，
// 第二次返回 skipped。总闸关闭时明确拒绝（而不是假装成功）。
func (h *WorkbuddyHandler) RunJob(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	job := chi.URLParam(r, "job")
	if !isWbJobKey(job) {
		resp.BadRequest(w, "未知任务: "+job)
		return
	}
	if h.Runtime == nil {
		resp.Internal(w, "运行态未初始化")
		return
	}
	var body struct {
		ProviderID int64 `json:"provider_id"`
	}
	if r.ContentLength > 0 {
		if err := resp.Decode(r, &body); err != nil {
			resp.BadRequest(w, "请求体格式错误")
			return
		}
	}
	results, err := h.Runtime.RunJobs(r.Context(), userID, job, body.ProviderID, "manual")
	if err != nil {
		// 总闸关闭 / 触发条件不满足：403 更贴合"被策略拒绝"的语义
		resp.Error(w, http.StatusForbidden, http.StatusForbidden, err.Error())
		return
	}
	resp.Success(w, map[string]any{"results": results})
}

func (h *WorkbuddyHandler) httpClient() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// tokenSet 解密渠道凭据为 TokenSet；任何一步失败返回 nil（不回显原因，避免泄露细节）。
func (h *WorkbuddyHandler) tokenSet(p *model.Provider) *oauth.TokenSet {
	cred, err := h.Op.GetCredentialByProviderID(p.ID)
	if err != nil || cred == nil {
		return nil
	}
	plain, err := crypto.Decrypt(cred.EncData)
	if err != nil {
		return nil
	}
	var d oauth.TokenSetData
	if err := json.Unmarshal(plain, &d); err != nil || d.AccessToken == "" {
		return nil
	}
	return &oauth.TokenSet{
		AccessToken:  d.AccessToken,
		RefreshToken: d.RefreshToken,
		ExpiresAt:    d.ExpiresAt,
		Extra:        d.Extra,
	}
}

func validWbProduct(v string) bool {
	switch v {
	case workbuddy.ProductWorkbuddy, workbuddy.ProductVSCode, workbuddy.ProductCLI:
		return true
	}
	return false
}

func validWbScrub(v string) bool {
	switch v {
	case workbuddy.ScrubOff, workbuddy.ScrubShadow, workbuddy.ScrubOn:
		return true
	}
	return false
}

func isWbJobKey(k string) bool {
	for _, s := range workbuddyJobKeys {
		if s == k {
			return true
		}
	}
	return false
}
