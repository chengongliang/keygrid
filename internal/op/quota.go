package op

import (
	"time"

	"github.com/chengongliang/keygrid/internal/model"

	"gorm.io/gorm/clause"
)

// quota.go OAuth 渠道额度快照（quota_snapshots）读写。
// 隔离约定：用户侧查询强制 user_id；被动观察为平台发起，走 Global 变体
// （同 ListRefreshDueCredentials / GetProviderGlobal 先例）。

// UpsertQuotaSnapshot 全量替换式 upsert（成功同步用，provider_id 唯一冲突时覆盖）。
func (o *Op) UpsertQuotaSnapshot(s *model.QuotaSnapshot) error {
	return o.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "provider_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"user_id", "platform", "status", "error", "data", "source", "fetched_at", "updated_at",
		}),
	}).Create(s).Error
}

// GetQuotaSnapshot 用户侧单渠道快照（user_id 硬隔离）。
func (o *Op) GetQuotaSnapshot(userID, providerID int64) (*model.QuotaSnapshot, error) {
	var s model.QuotaSnapshot
	if err := o.DB.Where("provider_id = ? AND user_id = ?", providerID, userID).First(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

// ListQuotaSnapshots 用户侧全部快照（渠道列表页额度区块一次拉全）。
func (o *Op) ListQuotaSnapshots(userID int64) ([]model.QuotaSnapshot, error) {
	var ss []model.QuotaSnapshot
	err := o.DB.Where("user_id = ?", userID).Find(&ss).Error
	return ss, err
}

// GetQuotaSnapshotGlobal 内部路径（被动观察合并）按 provider_id 取，不带 user 上下文。
func (o *Op) GetQuotaSnapshotGlobal(providerID int64) (*model.QuotaSnapshot, error) {
	var s model.QuotaSnapshot
	if err := o.DB.Where("provider_id = ?", providerID).First(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

// MarkQuotaSnapshotError 同步失败标记：已有快照时保留 data 只改状态（前端继续展示
// 上次成功数据 + 错误提示）；无快照则插入错误占位。
func (o *Op) MarkQuotaSnapshotError(providerID, userID int64, platform, msg string, now time.Time) error {
	res := o.DB.Model(&model.QuotaSnapshot{}).Where("provider_id = ?", providerID).
		Updates(map[string]any{"status": "error", "error": msg, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return o.DB.Create(&model.QuotaSnapshot{
		ProviderID: providerID,
		UserID:     userID,
		Platform:   platform,
		Status:     "error",
		Error:      msg,
		FetchedAt:  now,
	}).Error
}
