package op

import (
	"errors"
	"time"

	"github.com/chengongliang/keygrid/internal/model"

	"gorm.io/gorm"
)

var ErrNotFound = gorm.ErrRecordNotFound

type Op struct {
	DB *gorm.DB

	// 平台出口代理 30s TTL 缓存（op/proxy.go ProxyURL；转发热路径用）
	proxyCache *proxyCache
}

// ---- users ----

func (o *Op) CreateUser(u *model.User) error {
	if u.OidcSub != nil && *u.OidcSub == "" {
		u.OidcSub = nil // 空串会撞唯一索引（PG 只豁免 NULL 不豁免空串）
	}
	return o.DB.Create(u).Error
}

func (o *Op) GetUserByEmail(email string) (*model.User, error) {
	var u model.User
	if err := o.DB.Where("email = ?", email).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func (o *Op) GetUser(id int64) (*model.User, error) {
	var u model.User
	if err := o.DB.First(&u, id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// SetPassword 自助修改密码：只更新 password_hash（明文由 handler bcrypt 后传入），
// 不改 status/role —— 自助改密不是解锁手段（管理员解锁走 AdminSetPassword）。
func (o *Op) SetPassword(id int64, passwordHash string) error {
	return o.DB.Model(&model.User{}).Where("id = ?", id).Update("password_hash", passwordHash).Error
}

// ---- api_keys ----

func (o *Op) CreateApiKey(k *model.ApiKey) error {
	return o.DB.Create(k).Error
}

// GetApiKeyByKeyHash 鉴权路径专用：按 sha256 查 key。
func (o *Op) GetApiKeyByKeyHash(hash string) (*model.ApiKey, error) {
	var k model.ApiKey
	if err := o.DB.Where("key_hash = ?", hash).First(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

func (o *Op) ListApiKeys(userID int64) ([]model.ApiKey, error) {
	var ks []model.ApiKey
	err := o.DB.Where("user_id = ?", userID).Order("id DESC").Find(&ks).Error
	return ks, err
}

// GetApiKeyGlobal quota 检查回填路径专用：按 id 查 key（relay 路径 key 已由 ApiKeyAuth
// 鉴权；与 GetCredentialByProviderID 同理，不破坏 relay 侧 user_id 隔离）。
func (o *Op) GetApiKeyGlobal(id int64) (*model.ApiKey, error) {
	var k model.ApiKey
	if err := o.DB.First(&k, id).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// ResetApiKeyQuota 重置本人 key 的已用额度（quota_used 清零；Redis 键由调用方删）。
func (o *Op) ResetApiKeyQuota(userID, id int64) error {
	res := o.DB.Model(&model.ApiKey{}).Where("id = ? AND user_id = ?", id, userID).
		Update("quota_used", 0)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetApiKey 查本人单个 key（只读）。
func (o *Op) GetApiKey(userID, id int64) (*model.ApiKey, error) {
	var k model.ApiKey
	if err := o.DB.Where("id = ? AND user_id = ?", id, userID).First(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// UpdateApiKey load-then-save： loads the user's key, applies fn, saves.
// 走结构体 Save，避免 map Updates 绕过字段默认值/序列化。
func (o *Op) UpdateApiKey(userID, id int64, fn func(k *model.ApiKey)) (*model.ApiKey, error) {
	var k model.ApiKey
	if err := o.DB.Where("id = ? AND user_id = ?", id, userID).First(&k).Error; err != nil {
		return nil, err
	}
	fn(&k)
	if err := o.DB.Save(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// TouchApiKeyLastUsed relay 鉴权路径更新最后使用时间（节流由调用方控制）。
func (o *Op) TouchApiKeyLastUsed(id int64, t time.Time) error {
	return o.DB.Model(&model.ApiKey{}).Where("id = ?", id).
		Update("last_used_at", &t).Error
}

func (o *Op) DeleteApiKey(userID, id int64) error {
	res := o.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.ApiKey{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("api key not found")
	}
	return nil
}

// ---- providers（所有查询强制 user_id 隔离）----

func (o *Op) CreateProvider(p *model.Provider) error {
	return o.DB.Create(p).Error
}

func (o *Op) ListProviders(userID int64) ([]model.Provider, error) {
	var ps []model.Provider
	err := o.DB.Where("user_id = ?", userID).Order("id DESC").Find(&ps).Error
	return ps, err
}

func (o *Op) GetProvider(userID, id int64) (*model.Provider, error) {
	var p model.Provider
	if err := o.DB.Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateProvider loads the user's provider, applies fn, and saves it.
// Load-then-save keeps GORM serializers (model_map jsonb) correct, unlike
// map-based Updates which bypass struct field serializers.
func (o *Op) UpdateProvider(userID, id int64, fn func(p *model.Provider)) (*model.Provider, error) {
	var p model.Provider
	if err := o.DB.Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
		return nil, err
	}
	fn(&p)
	if err := o.DB.Save(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (o *Op) DeleteProvider(userID, id int64) error {
	return o.DB.Transaction(func(tx *gorm.DB) error {
		// 先找到 provider（校验归属），再级联删 credential
		var p model.Provider
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
			return err
		}
		if err := tx.Where("provider_id = ?", p.ID).Delete(&model.Credential{}).Error; err != nil {
			return err
		}
		return tx.Delete(&p).Error
	})
}

// ---- credentials ----

func (o *Op) UpsertCredential(c *model.Credential) error {
	var existing model.Credential
	err := o.DB.Where("provider_id = ?", c.ProviderID).First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return o.DB.Create(c).Error
		}
		return err
	}
	c.ID = existing.ID
	return o.DB.Save(c).Error
}

// GetCredentialByProviderID relay 转发路径取凭据用。
func (o *Op) GetCredentialByProviderID(providerID int64) (*model.Credential, error) {
	var c model.Credential
	if err := o.DB.Where("provider_id = ?", providerID).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// CredentialStatusByProviderIDs 批量取凭据状态（provider 列表"待授权"标记用）。
// 返回 provider_id → status；无凭据的 id 不在 map 中。
func (o *Op) CredentialStatusByProviderIDs(providerIDs []int64) (map[int64]string, error) {
	m := make(map[int64]string, len(providerIDs))
	if len(providerIDs) == 0 {
		return m, nil
	}
	var cs []model.Credential
	if err := o.DB.Where("provider_id IN ?", providerIDs).Find(&cs).Error; err != nil {
		return nil, err
	}
	for _, c := range cs {
		m[c.ProviderID] = c.Status
	}
	return m, nil
}

// ---- credentials: oauth 刷新相关 ----

// GetProviderGlobal 后台刷新路径用：按 id 查 provider（刷新调度由平台发起，
// 不带用户上下文；credential→provider 是 1:1 外键，不破坏 relay 侧 user_id 隔离）。
func (o *Op) GetProviderGlobal(id int64) (*model.Provider, error) {
	var p model.Provider
	if err := o.DB.First(&p, id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateCredential 保存刷新后的凭据（明文已由调用方加密）。
func (o *Op) UpdateCredential(c *model.Credential) error {
	return o.DB.Save(c).Error
}

// MarkCredentialError 可重试错误：记录 last_error，保留原 token。
func (o *Op) MarkCredentialError(credID int64, msg string) error {
	return o.DB.Model(&model.Credential{}).Where("id = ?", credID).
		Updates(map[string]any{"last_error": msg}).Error
}

// MarkCredentialRevoked 不可恢复错误：凭据置 revoked，用户需重新授权。
func (o *Op) MarkCredentialRevoked(credID int64, reason string) error {
	return o.DB.Model(&model.Credential{}).Where("id = ?", credID).
		Updates(map[string]any{"status": "revoked", "last_error": reason}).Error
}

// ListRefreshDueCredentials 后台扫描：oauth 凭据中 expires_at < deadline 且 status=active 的。
// api_key 凭据 expires_at 为 NULL，天然排除；refresh_token 为空的单个跳过（RefreshCredential 内判定）。
func (o *Op) ListRefreshDueCredentials(deadline time.Time) ([]model.Credential, error) {
	var cs []model.Credential
	err := o.DB.
		Joins("JOIN providers ON providers.id = credentials.provider_id").
		Where("providers.kind = ? AND providers.oauth_provider <> ''", "oauth").
		Where("credentials.status = ?", "active").
		Where("credentials.expires_at IS NOT NULL AND credentials.expires_at < ?", deadline).
		Find(&cs).Error
	return cs, err
}

// ---- oauth_states（授权流程临时态）----

// CreateOAuthState 一次性 state（5-10min 过期）。
func (o *Op) CreateOAuthState(s *model.OAuthState) error {
	return o.DB.Create(s).Error
}

// GetOAuthState 按 state 主键查。过期记录视为不存在（顺带清理）。
func (o *Op) GetOAuthState(state string) (*model.OAuthState, error) {
	var s model.OAuthState
	if err := o.DB.Where("state = ?", state).First(&s).Error; err != nil {
		return nil, err
	}
	if time.Now().After(s.ExpiresAt) {
		_ = o.DB.Delete(&s).Error
		return nil, gorm.ErrRecordNotFound
	}
	return &s, nil
}

// GetLatestOAuthState 该用户某 oauth provider key 最近一次未消费的授权 flow。
// Poll 接口用（state 表通过 provider_key 关联 provider.oauth_provider）。
func (o *Op) GetLatestOAuthState(userID int64, providerKey string) (*model.OAuthState, error) {
	var s model.OAuthState
	err := o.DB.
		Where("user_id = ? AND provider_key = ?", userID, providerKey).
		Where("expires_at > ?", time.Now()).
		Order("created_at DESC").
		First(&s).Error
	return &s, err
}

// UpdateOAuthStateTemp 回调后把 code 写回 temp。
func (o *Op) UpdateOAuthStateTemp(s *model.OAuthState) error {
	return o.DB.Model(&model.OAuthState{}).Where("state = ?", s.State).
		Updates(map[string]any{"temp": s.Temp}).Error
}

// DeleteOAuthState state 一次性消费。
func (o *Op) DeleteOAuthState(state string) error {
	return o.DB.Where("state = ?", state).Delete(&model.OAuthState{}).Error
}

// CleanupExpiredOAuthStates 定期清理过期 state。
func (o *Op) CleanupExpiredOAuthStates() error {
	return o.DB.Where("expires_at < ?", time.Now()).Delete(&model.OAuthState{}).Error
}

// ---- oidc_states（SSO 登录流程临时态）----

// CreateOidcState login 时创建一次性 state。
func (o *Op) CreateOidcState(s *model.OidcState) error {
	return o.DB.Create(s).Error
}

// ConsumeOidcState callback 消费 state：取出后立即删除（一次性），过期视为不存在。
func (o *Op) ConsumeOidcState(state string) (*model.OidcState, error) {
	var s model.OidcState
	if err := o.DB.Where("state = ?", state).First(&s).Error; err != nil {
		return nil, err
	}
	// 消费即删：无论后续校验是否通过，同一个 state 只能用一次
	_ = o.DB.Delete(&s).Error
	if time.Now().After(s.ExpiresAt) {
		return nil, gorm.ErrRecordNotFound
	}
	return &s, nil
}

// CleanupExpiredOidcStates 定期清理过期 state。
func (o *Op) CleanupExpiredOidcStates() error {
	return o.DB.Where("expires_at < ?", time.Now()).Delete(&model.OidcState{}).Error
}

// ---- users: OIDC 相关----

// GetUserByOidcSub 按 IdP subject 查用户（SSO 回来先按 sub 精确匹配）。
func (o *Op) GetUserByOidcSub(sub string) (*model.User, error) {
	var u model.User
	if err := o.DB.Where("oidc_sub = ?", sub).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// BindOidcSub 给已有用户绑定 oidc_sub（邮箱相同 = 同一人）。
func (o *Op) BindOidcSub(userID int64, sub string) error {
	return o.DB.Model(&model.User{}).Where("id = ?", userID).
		Update("oidc_sub", sub).Error
}

// TouchLastLogin 更新最近登录时间（本地/SSO 都记）。
func (o *Op) TouchLastLogin(userID int64) error {
	now := time.Now()
	return o.DB.Model(&model.User{}).Where("id = ?", userID).
		Update("last_login_at", &now).Error
}

// CreateUsageLogs 批量插入用量记录（relay 记账用，跨用户写入由 relay 层保证归属）。
func (o *Op) CreateUsageLogs(logs []*model.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	return o.DB.Create(&logs).Error
}

// FlushUsageBatch 记账 flush 事务：批量插入 usage_logs + 按 key 增量累计 quota_used
// （api_keys 的 DB 权威值；Redis 实时计数由 relay 层负责）。跨用户写入由 relay 层保证归属。
func (o *Op) FlushUsageBatch(logs []*model.UsageLog, quotaDeltas map[int64]float64) error {
	if len(logs) == 0 && len(quotaDeltas) == 0 {
		return nil
	}
	return o.DB.Transaction(func(tx *gorm.DB) error {
		if len(logs) > 0 {
			if err := tx.Create(&logs).Error; err != nil {
				return err
			}
		}
		for keyID, d := range quotaDeltas {
			if d == 0 {
				continue
			}
			if err := tx.Model(&model.ApiKey{}).Where("id = ?", keyID).
				Update("quota_used", gorm.Expr("quota_used + ?", d)).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// UsageFilter 用量筛选维度：0/空 = 不过滤（本人全量）。
type UsageFilter struct {
	ApiKeyID   int64  // 按平台签发的 API Key 过滤
	ProviderID int64  // 按渠道过滤（provider_id）
	Model      string // 按模型名过滤
}

func (f UsageFilter) apply(q *gorm.DB, tbl string) *gorm.DB {
	if f.ApiKeyID > 0 {
		q = q.Where(tbl+".api_key_id = ?", f.ApiKeyID)
	}
	if f.ProviderID > 0 {
		q = q.Where(tbl+".provider_id = ?", f.ProviderID)
	}
	if f.Model != "" {
		// 模型筛选按归一化计费名（billing_model 优先），与聚合分组口径一致
		q = q.Where("COALESCE(NULLIF("+tbl+".billing_model,''), "+tbl+".model) = ?", f.Model)
	}
	return q
}

// AggregateUsage 按日/模型聚合本人用量。
// days: 0=全部; from/to 优先于 days（YYYY-MM-DD 或 RFC3339）。返回按天+key+模型分组的 tokens/请求数/延迟。
type UsageAggRow struct {
	Day              string  `json:"day"`
	ApiKeyID         int64   `json:"api_key_id"`
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	Cost             float64 `json:"cost"` // USD（请求时价格快照累计）
}

func (o *Op) AggregateUsage(userID int64, days int, from, to *time.Time, f UsageFilter) ([]UsageAggRow, error) {
	// 计费名归一化：分组/筛选按 COALESCE(NULLIF(billing_model,''), model)，避免同一
	// 标准模型被渠道私有别名（BillingMap）拆成多行；NULLIF 处理空串（billing_model 默认 ''）。
	// 明细行保留真实请求名。
	q := o.DB.Model(&model.UsageLog{}).
		Select(
			"TO_CHAR(created_at AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD') as day, "+
				"api_key_id, COALESCE(NULLIF(billing_model,''), model) as model, COUNT(*) as requests, "+
				"COALESCE(SUM(prompt_tokens),0) as prompt_tokens, "+
				"COALESCE(SUM(completion_tokens),0) as completion_tokens, "+
				"COALESCE(AVG(latency_ms),0) as avg_latency_ms, "+
				"COALESCE(SUM(cost),0) as cost",
		).
		Where("user_id = ?", userID)
	q = ApplyUsageRange(q, "created_at", days, from, to)
	q = f.apply(q, "usage_logs")
	q = q.Group("day, api_key_id, COALESCE(NULLIF(billing_model,''), model)").Order("day DESC, model")
	var rows []UsageAggRow
	err := q.Scan(&rows).Error
	return rows, err
}

// UsageHourRow 分时活跃行（星期 × 小时聚合，热力图用）。
type UsageHourRow struct {
	Dow              int   `json:"dow"`  // 0=周日 … 6=周六（EXTRACT(DOW)）
	Hour             int   `json:"hour"` // 0-23
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

func (o *Op) aggregateUsageHourly(q *gorm.DB) ([]UsageHourRow, error) {
	// 与 AggregateUsage 同一时区处理：统一到 Asia/Shanghai 再取星期/小时
	q = q.Select(
		"EXTRACT(DOW FROM created_at AT TIME ZONE 'Asia/Shanghai')::int as dow, " +
			"EXTRACT(HOUR FROM created_at AT TIME ZONE 'Asia/Shanghai')::int as hour, " +
			"COUNT(*) as requests, " +
			"COALESCE(SUM(prompt_tokens),0) as prompt_tokens, " +
			"COALESCE(SUM(completion_tokens),0) as completion_tokens",
	).Group("dow, hour")
	var rows []UsageHourRow
	err := q.Scan(&rows).Error
	return rows, err
}

// AggregateUsageHourly 本人分时活跃聚合。days: 0=全部；from/to 优先于 days。
func (o *Op) AggregateUsageHourly(userID int64, days int, from, to *time.Time, f UsageFilter) ([]UsageHourRow, error) {
	q := o.DB.Model(&model.UsageLog{}).Where("user_id = ?", userID)
	q = ApplyUsageRange(q, "created_at", days, from, to)
	q = f.apply(q, "usage_logs")
	return o.aggregateUsageHourly(q)
}

// UsageByKeyRow 按 API Key 聚合的用量行（Key 用量汇总表用）。
// key_name/key_prefix 来自 LEFT JOIN api_keys；Key 已删除时为空串（前端回退显示）。
type UsageByKeyRow struct {
	ApiKeyID         int64      `json:"api_key_id"`
	KeyName          string     `json:"key_name"`
	KeyPrefix        string     `json:"key_prefix"`
	Requests         int64      `json:"requests"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	AvgLatencyMs     float64    `json:"avg_latency_ms"`
	Cost             float64    `json:"cost"`
	LastUsedAt       *time.Time `json:"last_used_at"`
}

// UsageByModelRow 按模型聚合的用量行（模型用量汇总表 + 模型筛选下拉用）。
type UsageByModelRow struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	Cost             float64 `json:"cost"`
}

// AggregateUsageByKey 按 API Key 汇总本人用量（不按 key 过滤，可带 model 过滤）。
func (o *Op) AggregateUsageByKey(userID int64, days int, from, to *time.Time, modelFlt string) ([]UsageByKeyRow, error) {
	q := o.DB.Model(&model.UsageLog{}).
		Select(
			"usage_logs.api_key_id as api_key_id, "+
				"COALESCE(MAX(api_keys.name),'') as key_name, "+
				"COALESCE(MAX(api_keys.prefix),'') as key_prefix, "+
				"COUNT(*) as requests, "+
				"COALESCE(SUM(usage_logs.prompt_tokens),0) as prompt_tokens, "+
				"COALESCE(SUM(usage_logs.completion_tokens),0) as completion_tokens, "+
				"COALESCE(AVG(usage_logs.latency_ms),0) as avg_latency_ms, "+
				"COALESCE(SUM(usage_logs.cost),0) as cost, "+
				"MAX(usage_logs.created_at) as last_used_at",
		).
		Joins("LEFT JOIN api_keys ON api_keys.id = usage_logs.api_key_id").
		Where("usage_logs.user_id = ?", userID)
	q = ApplyUsageRange(q, "usage_logs.created_at", days, from, to)
	if modelFlt != "" {
		q = q.Where("usage_logs.model = ?", modelFlt)
	}
	q = q.Group("usage_logs.api_key_id").
		Order("COALESCE(SUM(usage_logs.prompt_tokens),0) + COALESCE(SUM(usage_logs.completion_tokens),0) DESC")
	var rows []UsageByKeyRow
	err := q.Scan(&rows).Error
	return rows, err
}

// AggregateUsageByModel 按模型汇总本人用量（不按模型过滤，可带 key 过滤）。
func (o *Op) AggregateUsageByModel(userID int64, days int, from, to *time.Time, apiKeyID int64) ([]UsageByModelRow, error) {
	q := o.DB.Model(&model.UsageLog{}).
		Select(
			"COALESCE(NULLIF(billing_model,''), model) as model, COUNT(*) as requests, "+
				"COALESCE(SUM(prompt_tokens),0) as prompt_tokens, "+
				"COALESCE(SUM(completion_tokens),0) as completion_tokens, "+
				"COALESCE(AVG(latency_ms),0) as avg_latency_ms, "+
				"COALESCE(SUM(cost),0) as cost",
		).
		Where("user_id = ?", userID)
	q = ApplyUsageRange(q, "created_at", days, from, to)
	if apiKeyID > 0 {
		q = q.Where("api_key_id = ?", apiKeyID)
	}
	q = q.Group("COALESCE(NULLIF(billing_model,''), model)").
		Order("COALESCE(SUM(prompt_tokens),0) + COALESCE(SUM(completion_tokens),0) DESC")
	var rows []UsageByModelRow
	err := q.Scan(&rows).Error
	return rows, err
}

// UsageByProviderRow 按渠道聚合的用量行（渠道用量表用）。
// name/kind/protocol/oauth_provider 来自 LEFT JOIN providers；渠道已删除时为空串（前端回退显示）。
// OAuthProvider 显式列名：同 model.Provider，GORM 默认策略会拆成 o_auth_provider 导致 Scan 不匹配。
type UsageByProviderRow struct {
	ProviderID       int64      `json:"provider_id"`
	Name             string     `json:"name"`
	Kind             string     `json:"kind"`
	Protocol         string     `json:"protocol"`
	OAuthProvider    string     `gorm:"column:oauth_provider" json:"oauth_provider,omitempty"`
	Requests         int64      `json:"requests"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	AvgLatencyMs     float64    `json:"avg_latency_ms"`
	Cost             float64    `json:"cost"`
	LastUsedAt       *time.Time `json:"last_used_at"`
}

// AggregateUsageByProvider 按渠道汇总本人用量（不按 provider 过滤，可带 key/model 过滤）。
// 与 relay 路由同口径：只能在 user_id 隔离下的记录里聚合，且只取元数据（tokens/费用/状态）。
func (o *Op) AggregateUsageByProvider(userID int64, days int, from, to *time.Time, f UsageFilter) ([]UsageByProviderRow, error) {
	q := o.DB.Model(&model.UsageLog{}).
		Select(
			"usage_logs.provider_id as provider_id, "+
				"COALESCE(MAX(providers.name),'') as name, "+
				"COALESCE(MAX(providers.kind),'') as kind, "+
				"COALESCE(MAX(providers.protocol),'') as protocol, "+
				"COALESCE(MAX(providers.oauth_provider),'') as oauth_provider, "+
				"COUNT(*) as requests, "+
				"COALESCE(SUM(usage_logs.prompt_tokens),0) as prompt_tokens, "+
				"COALESCE(SUM(usage_logs.completion_tokens),0) as completion_tokens, "+
				"COALESCE(AVG(usage_logs.latency_ms),0) as avg_latency_ms, "+
				"COALESCE(SUM(usage_logs.cost),0) as cost, "+
				"MAX(usage_logs.created_at) as last_used_at",
		).
		Joins("LEFT JOIN providers ON providers.id = usage_logs.provider_id").
		Where("usage_logs.user_id = ?", userID)
	q = ApplyUsageRange(q, "usage_logs.created_at", days, from, to)
	q = f.apply(q, "usage_logs")
	q = q.Group("usage_logs.provider_id").
		Order("COALESCE(SUM(usage_logs.prompt_tokens),0) + COALESCE(SUM(usage_logs.completion_tokens),0) DESC")
	var rows []UsageByProviderRow
	err := q.Scan(&rows).Error
	return rows, err
}

// UsageCostSummary 用户侧费用汇总（/api/usage/summary；只展示不拦截 —— 额度硬限只在 key 级）。
type UsageCostSummary struct {
	TotalCost float64 `json:"total_cost"` // 累计总费用（全部历史）
	// Cost30d 显式列名：GORM 会把 Cost30d 默认转成 cost30d，与 SQL alias cost_30d 不匹配导致 Scan 恒零值
	Cost30d     float64 `gorm:"column:cost_30d" json:"cost_30d"` // 近 30 天费用
	TotalTokens int64   `json:"total_tokens"`                    // 累计 tokens（prompt+completion）
	Requests    int64   `json:"requests"`                        // 累计请求数
}

// UserCostSummary 本人费用汇总（user_id 隔离）。
func (o *Op) UserCostSummary(userID int64) (*UsageCostSummary, error) {
	var row UsageCostSummary
	err := o.DB.Model(&model.UsageLog{}).
		Select(
			"COALESCE(SUM(cost),0) as total_cost, "+
				"COALESCE(SUM(cost) FILTER (WHERE created_at > NOW() - INTERVAL '30 days'),0) as cost_30d, "+
				"COALESCE(SUM(prompt_tokens+completion_tokens),0) as total_tokens, "+
				"COUNT(*) as requests",
		).
		Where("user_id = ?", userID).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ListUserModels 聚合本人 enabled 渠道的模型列表（model_map keys；无 map 渠道无法枚举，跳过）。
// providerIDs 非空时仅统计白名单内渠道（key 级渠道绑定，nil/空 = 不限）。
func (o *Op) ListUserModels(userID int64, providerIDs []int64) ([]string, error) {
	q := o.DB.Where("user_id = ? AND enabled = ?", userID, true)
	if len(providerIDs) > 0 {
		q = q.Where("id IN ?", providerIDs)
	}
	var ps []model.Provider
	if err := q.Find(&ps).Error; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		for name := range p.ModelMap {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// ListAllUserModelNames 全平台所有 enabled 渠道的 model_map keys 去重（admin 未定价提醒用；
// 无 map 渠道枚举不出，结果标注"非完整"）。
func (o *Op) ListAllUserModelNames() ([]string, error) {
	var ps []model.Provider
	if err := o.DB.Where("enabled = ?", true).Find(&ps).Error; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		for name := range p.ModelMap {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out, nil
}
