package op

import (
	"time"

	"github.com/chengongliang/keygrid/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// workbuddy.go WorkBuddy / CodeBuddy 渠道特化的数据操作。
//
// 隔离约定（AGENTS.md 规则 1）：用户侧方法一律带 user_id；仅调度器需要的
// 平台级扫描走显式的 Global 变体，命名与 quota.go 的 GetQuotaSnapshotGlobal
// 先例一致，且在注释里写明用途与风险。

// ---- 用户设置 ----

// GetWorkbuddySetting 取用户设置；从未配置过时返回零值（Enabled=false），
// 不返回 ErrRecordNotFound —— 调用方需要的是"默认关"，不是错误分支。
func (o *Op) GetWorkbuddySetting(userID int64) (*model.WorkbuddySetting, error) {
	var s model.WorkbuddySetting
	err := o.DB.Where("user_id = ?", userID).First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return &model.WorkbuddySetting{UserID: userID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// UpsertWorkbuddySetting 保存用户设置（user_id 唯一冲突时覆盖）。
func (o *Op) UpsertWorkbuddySetting(s *model.WorkbuddySetting) error {
	return o.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"enabled", "reserve_credits", "daily_token_limit", "product",
			"scrub_mode", "fingerprint", "jobs_enabled", "updated_at",
		}),
	}).Create(s).Error
}

// ListWorkbuddySettingsWithJobs 平台级扫描：所有开启了自动化任务总开关的用户设置。
//
// 仅后台调度器使用（对齐 ListRefreshDueCredentials 的先例）。返回结果里不含
// 任何凭据；调用方按 user_id 继续展开各自渠道。
func (o *Op) ListWorkbuddySettingsWithJobs() ([]model.WorkbuddySetting, error) {
	var ss []model.WorkbuddySetting
	err := o.DB.Where("enabled = ? AND jobs_enabled IS NOT NULL", true).Find(&ss).Error
	return ss, err
}

// ---- 渠道运行态 ----

// ListWorkbuddyChannelStates 用户侧全部渠道运行态（渠道列表页一次拉全）。
func (o *Op) ListWorkbuddyChannelStates(userID int64) ([]model.WorkbuddyChannelState, error) {
	var ss []model.WorkbuddyChannelState
	err := o.DB.Where("user_id = ?", userID).Find(&ss).Error
	return ss, err
}

// GetWorkbuddyChannelState 用户侧单渠道运行态（user_id 硬隔离）。
func (o *Op) GetWorkbuddyChannelState(userID, providerID int64) (*model.WorkbuddyChannelState, error) {
	var s model.WorkbuddyChannelState
	err := o.DB.Where("provider_id = ? AND user_id = ?", providerID, userID).First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// UpsertWorkbuddyChannelState 保存渠道运行态（provider_id 唯一冲突时覆盖）。
// user_id 由调用方从候选渠道带出，不会被上游请求参数覆盖（安全前提）。
func (o *Op) UpsertWorkbuddyChannelState(s *model.WorkbuddyChannelState) error {
	return o.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "provider_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"user_id", "realm", "account_uid", "nickname", "credits", "credits_at",
			"daily_tokens", "tokens_date", "parked_reason", "cooldowns", "last_ok_at",
			"meta", "updated_at",
		}),
	}).Create(s).Error
}

// SetWorkbuddyCredits 更新积分观测值（无运行态行时自动建行，见 upsertChannelStateFields）。
func (o *Op) SetWorkbuddyCredits(userID, providerID int64, credits float64, at time.Time) error {
	return o.upsertChannelStateFields(userID, providerID, map[string]any{
		"credits": credits, "credits_at": at,
	}, at)
}

// SetWorkbuddyParked 标记/解除渠道停用（reason 为空串表示恢复正常）。
func (o *Op) SetWorkbuddyParked(userID, providerID int64, reason string, now time.Time) error {
	return o.upsertChannelStateFields(userID, providerID, map[string]any{
		"parked_reason": reason,
	}, now)
}

// upsertChannelStateFields 「只写几个字段」的运行态更新：行不存在时先补一行。
//
// 为什么需要：积分刷新、停用标记等路径可能先于任何转发请求发生（新接入渠道、
// 手动点「刷新积分」、后台任务首次执行）。这些路径用 Model+Where.Updates 时，
// 行不存在则 RowsAffected=0 且 err=nil —— 调用方以为成功，值却静默丢失。
// 因此统一走「先更新 → 未命中则建行（ON CONFLICT DO NOTHING）→ 再更新」。
//
// realm 只在建行时留空：本层不能 import workbuddy（会形成循环依赖），
// 下一次转发请求的 UpsertWorkbuddyChannelState 会按渠道 oauth_provider 补齐；
// realm 仅用于展示，留空不影响任何判定。
func (o *Op) upsertChannelStateFields(userID, providerID int64, fields map[string]any, now time.Time) error {
	return o.DB.Transaction(func(tx *gorm.DB) error {
		fields["updated_at"] = now
		res := tx.Model(&model.WorkbuddyChannelState{}).
			Where("provider_id = ? AND user_id = ?", providerID, userID).
			Updates(fields)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			return nil
		}
		// 行不存在：建一行（并发下由 ON CONFLICT 兜住），随后重放本次更新。
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "provider_id"}},
			DoNothing: true,
		}).Create(&model.WorkbuddyChannelState{
			UserID:     userID,
			ProviderID: providerID,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&model.WorkbuddyChannelState{}).
			Where("provider_id = ? AND user_id = ?", providerID, userID).
			Updates(fields).Error
	})
}

// AddWorkbuddyDailyTokens 累加当日 token 用量；跨天时先清零再累加。
//
// 权威值仍是 usage_logs 的聚合（SumProviderTokens），这里是热路径缓存：
// 每次记账后调用，避免每个请求都做一次聚合查询。
func (o *Op) AddWorkbuddyDailyTokens(userID, providerID int64, date string, delta int64, now time.Time) error {
	return o.DB.Transaction(func(tx *gorm.DB) error {
		var s model.WorkbuddyChannelState
		err := tx.Where("provider_id = ? AND user_id = ?", providerID, userID).First(&s).Error
		if err == gorm.ErrRecordNotFound {
			// 运行态行还没建（渠道刚接入就被调用）：建一行，带上本次用量。
			return tx.Create(&model.WorkbuddyChannelState{
				UserID:      userID,
				ProviderID:  providerID,
				DailyTokens: delta,
				TokensDate:  date,
			}).Error
		}
		if err != nil {
			return err
		}
		total := s.DailyTokens + delta
		if s.TokensDate != date {
			total = delta // 跨天：昨天的不计入今天
		}
		return tx.Model(&model.WorkbuddyChannelState{}).
			Where("provider_id = ? AND user_id = ?", providerID, userID).
			Updates(map[string]any{"daily_tokens": total, "tokens_date": date, "updated_at": now}).Error
	})
}

// UpdateWorkbuddyMeta 合并写入一个 Meta 键（任务幂等标记、事件去重池等）。
// 读改写在一个事务里完成，避免并发 Tick 互相覆盖。
func (o *Op) UpdateWorkbuddyMeta(userID, providerID int64, key string, value any, now time.Time) error {
	return o.DB.Transaction(func(tx *gorm.DB) error {
		var s model.WorkbuddyChannelState
		err := tx.Where("provider_id = ? AND user_id = ?", providerID, userID).First(&s).Error
		if err == gorm.ErrRecordNotFound {
			return tx.Create(&model.WorkbuddyChannelState{
				UserID:     userID,
				ProviderID: providerID,
				Meta:       map[string]any{key: value},
			}).Error
		}
		if err != nil {
			return err
		}
		if s.Meta == nil {
			s.Meta = map[string]any{}
		}
		s.Meta[key] = value
		return tx.Model(&model.WorkbuddyChannelState{}).
			Where("provider_id = ? AND user_id = ?", providerID, userID).
			Updates(map[string]any{"meta": s.Meta, "updated_at": now}).Error
	})
}

// ---- 任务执行日志 ----

// InsertWorkbuddyJobRun 落一条任务执行记录。
func (o *Op) InsertWorkbuddyJobRun(r *model.WorkbuddyJobRun) error {
	return o.DB.Create(r).Error
}

// ListWorkbuddyJobRuns 用户侧最近执行记录（面板一次拉 50 条）。
func (o *Op) ListWorkbuddyJobRuns(userID int64, limit int) ([]model.WorkbuddyJobRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rs []model.WorkbuddyJobRun
	err := o.DB.Where("user_id = ?", userID).
		Order("started_at DESC, id DESC").Limit(limit).Find(&rs).Error
	return rs, err
}

// PruneWorkbuddyJobRuns 清理过期执行记录（默认保留 30 天，后台任务调用）。
func (o *Op) PruneWorkbuddyJobRuns(before time.Time) (int64, error) {
	res := o.DB.Where("started_at < ?", before).Delete(&model.WorkbuddyJobRun{})
	return res.RowsAffected, res.Error
}

// ---- 用量聚合 ----

// SumProviderTokens 聚合某渠道在时间窗口内的 token 总量（prompt + completion）。
//
// 日限额的权威判定依据：usage_logs 是平台账本，不另立一套流水。
// 走 user_id + provider_id 双条件，隔离规则不破。
func (o *Op) SumProviderTokens(userID, providerID int64, since, until time.Time) (int64, error) {
	var sum struct {
		Total int64
	}
	err := o.DB.Model(&model.UsageLog{}).
		Select("COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS total").
		Where("user_id = ? AND provider_id = ? AND created_at >= ? AND created_at < ?",
			userID, providerID, since, until).
		Scan(&sum).Error
	return sum.Total, err
}
