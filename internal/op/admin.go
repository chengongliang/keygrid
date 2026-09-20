package op

import (
	"time"

	"github.com/chengongliang/keygrid/internal/model"

	"gorm.io/gorm"
)

// ---- admin.go：平台治理 op 层。用户管理 / 平台聚合 / 渠道健康 / kv 设置 / 邀请码 ----

// ---- 用户管理 ----

// AdminUserFilter 用户列表过滤条件。
type AdminUserFilter struct {
	Search string // email/name 模糊匹配
	Page   int    // 从 1 开始
	Size   int    // 默认 20，最大 100
}

// AdminUserRow 列表行（含 key 数，便于运营视角）。
type AdminUserRow struct {
	ID             int64      `json:"id"`
	Email          string     `json:"email"`
	Name           string     `json:"name"`
	Role           string     `json:"role"`
	Status         string     `json:"status"`
	ApiKeyCount    int64      `json:"api_key_count"`
	ProviderCount  int64      `json:"provider_count"`
	TotalRequests  int64      `json:"total_requests"`
	TotalTokens    int64      `json:"total_tokens"`
	TotalCost      float64    `json:"total_cost"`
	LastActivityAt *time.Time `json:"last_activity_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// AdminListUsers 分页 + 搜索（email/name ILIKE）。
func (o *Op) AdminListUsers(f AdminUserFilter) ([]AdminUserRow, int64, error) {
	size := f.Size
	if size <= 0 || size > 100 {
		size = 20
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}

	base := o.DB.Model(&model.User{})
	if f.Search != "" {
		like := "%" + f.Search + "%"
		base = base.Where("email ILIKE ? OR name ILIKE ?", like, like)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var users []model.User
	if err := base.Order("id ASC").Offset((page - 1) * size).Limit(size).Find(&users).Error; err != nil {
		return nil, 0, err
	}

	rows := make([]AdminUserRow, 0, len(users))
	for _, u := range users {
		row := AdminUserRow{
			ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, Status: u.Status, CreatedAt: u.CreatedAt,
		}
		o.DB.Model(&model.ApiKey{}).Where("user_id = ?", u.ID).Count(&row.ApiKeyCount)
		o.DB.Model(&model.Provider{}).Where("user_id = ?", u.ID).Count(&row.ProviderCount)
		agg := struct {
			Requests int64
			Tokens   int64
			Cost     float64
			Last     *time.Time
		}{}
		o.DB.Model(&model.UsageLog{}).Select(
			"COUNT(*) as requests, COALESCE(SUM(prompt_tokens+completion_tokens),0) as tokens, COALESCE(SUM(cost),0) as cost, MAX(created_at) as last",
		).Where("user_id = ?", u.ID).Scan(&agg)
		row.TotalRequests = agg.Requests
		row.TotalTokens = agg.Tokens
		row.TotalCost = agg.Cost
		row.LastActivityAt = agg.Last
		rows = append(rows, row)
	}
	return rows, total, nil
}

// AdminUpdateUser 载入-修改-保存（role/status 变更走同一入口）。
func (o *Op) AdminUpdateUser(id int64, fn func(u *model.User)) (*model.User, error) {
	var u model.User
	if err := o.DB.First(&u, id).Error; err != nil {
		return nil, err
	}
	fn(&u)
	if err := o.DB.Save(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// AdminDisableUser 禁用：置 status=disabled 并吊销其全部 API Key（JWT 由登录校验 + role claim 天然失效）。
func (o *Op) AdminDisableUser(id int64) error {
	return o.DB.Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.First(&u, id).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.User{}).Where("id = ?", id).Update("status", "disabled").Error; err != nil {
			return err
		}
		// 禁用后 API Key 全失效
		return tx.Model(&model.ApiKey{}).Where("user_id = ?", id).Update("enabled", false).Error
	})
}

// AdminEnableUser 启用：status=active（API Key 不自动恢复，由用户自行启用）。
func (o *Op) AdminEnableUser(id int64) error {
	return o.DB.Model(&model.User{}).Where("id = ?", id).Update("status", "active").Error
}

// AdminSetPassword 重置密码（明文由 handler bcrypt 后传入）。
// 顺带把账号恢复为 active —— 管理员重置密码即意味着解锁账号。
func (o *Op) AdminSetPassword(id int64, passwordHash string) error {
	return o.DB.Model(&model.User{}).Where("id = ?", id).
		Updates(map[string]any{"password_hash": passwordHash, "status": "active"}).Error
}

// CountAdmins 统计 admin 数（防止降级最后一个 admin）。
func (o *Op) CountAdmins() (int64, error) {
	var n int64
	err := o.DB.Model(&model.User{}).Where("role = ?", "admin").Count(&n).Error
	return n, err
}

// PromoteAdminByEmail 首个 admin 提升（启动时 ADMIN_EMAIL）。
func (o *Op) PromoteAdminByEmail(email string) (int64, error) {
	var u model.User
	if err := o.DB.Where("email = ?", email).First(&u).Error; err != nil {
		return 0, err
	}
	if u.Role != "admin" {
		if err := o.DB.Model(&u).Update("role", "admin").Error; err != nil {
			return 0, err
		}
	}
	return u.ID, nil
}

// ---- 全平台用量聚合 ----

// PlatformUsageAggRow 管理端聚合行（by=user 时多 user 维度字段）。
type PlatformUsageAggRow struct {
	Day              string  `json:"day"`
	UserID           int64   `json:"user_id"`
	Email            string  `json:"email"`
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	FailedRequests   int64   `json:"failed_requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	Cost             float64 `json:"cost"`
}

// AggregatePlatformUsage 全平台聚合：by = user | model | day。from/to 优先于 days（0 = 全部）。
func (o *Op) AggregatePlatformUsage(by string, days int, from, to *time.Time) ([]PlatformUsageAggRow, error) {
	metrics := "COUNT(*) as requests, " +
		"COUNT(*) FILTER (WHERE usage_logs.status_code >= 400) as failed_requests, " +
		"COALESCE(SUM(usage_logs.prompt_tokens),0) as prompt_tokens, " +
		"COALESCE(SUM(usage_logs.completion_tokens),0) as completion_tokens, " +
		"COALESCE(AVG(usage_logs.latency_ms),0) as avg_latency_ms, " +
		"COALESCE(SUM(usage_logs.cost),0) as cost"

	dayExpr := "TO_CHAR(usage_logs.created_at AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD')"
	var sel, group string
	switch by {
	case "user": // 按用户+日展开
		sel = dayExpr + " as day, usage_logs.user_id as user_id, COALESCE(MIN(u.email),'') as email, '' as model"
		group = dayExpr + ", usage_logs.user_id"
	case "model": // 按模型汇总（按归一化计费名归并，与用户侧口径一致；NULLIF 处理空串）
		sel = "'' as day, 0 as user_id, '' as email, COALESCE(NULLIF(usage_logs.billing_model,''), usage_logs.model) as model"
		group = "COALESCE(NULLIF(usage_logs.billing_model,''), usage_logs.model)"
	default: // day：按日全平台汇总
		sel = dayExpr + " as day, 0 as user_id, '' as email, '' as model"
		group = dayExpr
	}

	q := o.DB.Model(&model.UsageLog{}).
		Joins("JOIN users u ON u.id = usage_logs.user_id").
		Select(sel + ", " + metrics).
		Group(group).
		Order("day DESC, requests DESC")
	q = ApplyUsageRange(q, "usage_logs.created_at", days, from, to)

	var rows []PlatformUsageAggRow
	err := q.Scan(&rows).Error
	return rows, err
}

// AggregatePlatformUsageHourly 全平台分时活跃聚合（热力图用）。days: 0 = 全部；from/to 优先于 days。
func (o *Op) AggregatePlatformUsageHourly(days int, from, to *time.Time) ([]UsageHourRow, error) {
	q := o.DB.Model(&model.UsageLog{})
	q = ApplyUsageRange(q, "usage_logs.created_at", days, from, to)
	return o.aggregateUsageHourly(q)
}

// TopUsersByTokens TOP 用户排行（tokens 降序）。
type TopUserRow struct {
	UserID           int64   `json:"user_id"`
	Email            string  `json:"email"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	Cost             float64 `json:"cost"`
}

func (o *Op) TopUsersByTokens(days, limit int, from, to *time.Time) ([]TopUserRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	q := o.DB.Model(&model.UsageLog{}).
		Joins("JOIN users u ON u.id = usage_logs.user_id").
		Select(
			"usage_logs.user_id as user_id, COALESCE(MIN(u.email),'') as email, COUNT(*) as requests, " +
				"COALESCE(SUM(usage_logs.prompt_tokens),0) as prompt_tokens, " +
				"COALESCE(SUM(usage_logs.completion_tokens),0) as completion_tokens, " +
				"COALESCE(AVG(usage_logs.latency_ms),0) as avg_latency_ms, " +
				"COALESCE(SUM(usage_logs.cost),0) as cost",
		).
		Group("usage_logs.user_id").
		Order("(COALESCE(SUM(usage_logs.prompt_tokens),0)+COALESCE(SUM(usage_logs.completion_tokens),0)) DESC").
		Limit(limit)
	q = ApplyUsageRange(q, "usage_logs.created_at", days, from, to)
	var rows []TopUserRow
	err := q.Scan(&rows).Error
	return rows, err
}

// ---- 渠道健康（只读，绝不含凭据）----

// ProviderHealthRow 渠道健康行。
type ProviderHealthRow struct {
	ID            int64      `json:"id"`
	UserID        int64      `json:"user_id"`
	UserEmail     string     `json:"user_email"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind"`
	Protocol      string     `json:"protocol"`
	Enabled       bool       `json:"enabled"`
	BreakerCheck  bool       `json:"breaker_check"`
	CredStatus    string     `json:"cred_status"`
	CredExpiresAt *time.Time `json:"cred_expires_at"`
	LastError     string     `json:"last_error"`
	// 近 24h
	Req24h       int64   `json:"req_24h"`
	Fail24h      int64   `json:"fail_24h"`
	SuccessRate  float64 `json:"success_rate"` // 0-100
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	// 熔断快照由 handler 注入（进程内 Breaker）
	BreakerState string `json:"breaker_state"`
}

// ListProviderHealth 全部渠道健康概览（按 user_id + name 排序）。
func (o *Op) ListProviderHealth() ([]ProviderHealthRow, error) { return o.listProviderHealth(0) }

// ListProviderHealthForUser 用户端渠道可用性：只含该用户自己的渠道（数据隔离硬性规则）。
// 与 admin 版共用同一查询与 24h 聚合口径，保证两端看到同样的数字。
func (o *Op) ListProviderHealthForUser(userID int64) ([]ProviderHealthRow, error) {
	return o.listProviderHealth(userID)
}

// listProviderHealth userID > 0 时限定该用户的渠道；0 = 全平台（admin）。
func (o *Op) listProviderHealth(userID int64) ([]ProviderHealthRow, error) {
	var rows []ProviderHealthRow
	q := o.DB.Table("providers p").
		Select(
			"p.id as id, p.user_id as user_id, COALESCE(u.email,'') as user_email, p.name as name, p.kind as kind, p.protocol as protocol, p.enabled as enabled, p.breaker_check as breaker_check, " +
				"COALESCE(c.status,'') as cred_status, c.expires_at as cred_expires_at, COALESCE(c.last_error,'') as last_error",
		).
		Joins("JOIN users u ON u.id = p.user_id").
		Joins("LEFT JOIN credentials c ON c.provider_id = p.id")
	if userID > 0 {
		q = q.Where("p.user_id = ?", userID)
	}
	if err := q.Order("p.user_id ASC, p.id ASC").Scan(&rows).Error; err != nil {
		return nil, err
	}

	// 近 24h 聚合：一条 SQL 按 provider_id 汇总（用户端同样按 user_id 收窄，避免全表聚合）
	type aggRow struct {
		ProviderID int64
		Requests   int64
		Failed     int64
		AvgLatency float64
	}
	var aggs []aggRow
	aggQ := o.DB.Model(&model.UsageLog{}).
		Select("provider_id, COUNT(*) as requests, COUNT(*) FILTER (WHERE status_code >= 400) as failed, COALESCE(AVG(latency_ms),0) as avg_latency").
		Where("created_at > NOW() - INTERVAL '24 hours'")
	if userID > 0 {
		aggQ = aggQ.Where("user_id = ?", userID)
	}
	if err := aggQ.Group("provider_id").Scan(&aggs).Error; err != nil {
		return nil, err
	}
	aggMap := make(map[int64]aggRow, len(aggs))
	for _, a := range aggs {
		aggMap[a.ProviderID] = a
	}
	for i := range rows {
		a, ok := aggMap[rows[i].ID]
		if !ok {
			continue
		}
		rows[i].Req24h = a.Requests
		rows[i].Fail24h = a.Failed
		rows[i].AvgLatencyMs = a.AvgLatency
		if a.Requests > 0 {
			rows[i].SuccessRate = float64(a.Requests-a.Failed) / float64(a.Requests) * 100
		}
	}
	return rows, nil
}

// ---- platform_settings kv ----

// GetSetting 读 kv；不存在返回 ("", nil)。
func (o *Op) GetSetting(key string) (string, error) {
	var s model.PlatformSetting
	err := o.DB.Where("\"key\" = ?", key).First(&s).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return "", nil
		}
		return "", err
	}
	return s.Value, nil
}

// SetSetting upsert kv。
func (o *Op) SetSetting(key, value string) error {
	return o.DB.Save(&model.PlatformSetting{Key: key, Value: value, UpdatedAt: time.Now()}).Error
}

// GetAllSettings 一次读全部（设置页初始化用）。
func (o *Op) GetAllSettings() (map[string]string, error) {
	var ss []model.PlatformSetting
	if err := o.DB.Find(&ss).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ss))
	for _, s := range ss {
		out[s.Key] = s.Value
	}
	return out, nil
}

// GetMaintenanceMode 维护模式开关（relay 路径热路径用；缺失 = off）。
func (o *Op) GetMaintenanceMode() (bool, error) {
	v, err := o.GetSetting(model.SettingMaintenanceMode)
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// ---- 邀请码 ----

// CreateInvitation 生成邀请码（默认 14 天过期）。
func (o *Op) CreateInvitation(code string, createdBy int64, expiresAt time.Time) (*model.Invitation, error) {
	inv := &model.Invitation{Code: code, CreatedBy: createdBy, ExpiresAt: expiresAt}
	if err := o.DB.Create(inv).Error; err != nil {
		return nil, err
	}
	return inv, nil
}

// ListInvitations 最近邀请码（admin 页展示）。
func (o *Op) ListInvitations(limit int) ([]model.Invitation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var invs []model.Invitation
	err := o.DB.Order("id DESC").Limit(limit).Find(&invs).Error
	return invs, err
}

// ConsumeInvitation 注册路径消费邀请码：原子置 used_by 防并发复用。
func (o *Op) ConsumeInvitation(code string, userID int64) error {
	now := time.Now()
	res := o.DB.Model(&model.Invitation{}).
		Where("code = ? AND used_by IS NULL AND expires_at > ?", code, now).
		Updates(map[string]any{"used_by": userID, "used_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
