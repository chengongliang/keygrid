package op

import (
	"log"

	"github.com/chengongliang/keygrid/internal/model"
)

// CreateAuditLog 审计事件落库；失败只打日志，绝不阻塞主流程。
func (o *Op) CreateAuditLog(l *model.AuditLog) {
	if err := o.DB.Create(l).Error; err != nil {
		log.Printf("[audit] write failed: %v", err)
	}
}

// ListAuditLogs 本人审计日志（时间倒序，分页）。size 默认 20，最大 100。
func (o *Op) ListAuditLogs(userID int64, page, size int) ([]model.AuditLog, int64, error) {
	if size <= 0 || size > 100 {
		size = 20
	}
	page = max(page, 1)

	base := o.DB.Model(&model.AuditLog{}).Where("user_id = ?", userID)
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []model.AuditLog
	err := base.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&logs).Error
	return logs, total, err
}
