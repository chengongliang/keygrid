package op

import (
	"errors"

	"github.com/chengongliang/keygrid/internal/model"

	"gorm.io/gorm"
)

// pricing.go admin 全局模型价格表 CRUD（计费）。
// 价格是全局唯一来源：admin 维护；用户侧只读（/api/prices）。
// 渠道级计费名映射（BillingMap）的目标必须取自本表，防止映射逃费。

// ListModelPrices 全量价格表（admin 管理 + relay 内存缓存回源用；条目量级几百，全量读无压力）。
func (o *Op) ListModelPrices() ([]model.ModelPrice, error) {
	var ps []model.ModelPrice
	err := o.DB.Order("model ASC").Find(&ps).Error
	return ps, err
}

// UpsertModelPrice 按 model upsert（幂等；handler 层负责校验 model 非空、价格非负）。
func (o *Op) UpsertModelPrice(p *model.ModelPrice) error {
	var existing model.ModelPrice
	err := o.DB.Where("model = ?", p.Model).First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return o.DB.Create(p).Error
		}
		return err
	}
	p.ID = existing.ID
	p.CreatedAt = existing.CreatedAt
	return o.DB.Save(p).Error
}

// DeleteModelPrice 删除定价（该模型回到"未定价 = 免费"语义）。
func (o *Op) DeleteModelPrice(id int64) error {
	res := o.DB.Delete(&model.ModelPrice{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpsertModelPricesBatch 批量 upsert（同步导入用）：单事务，任一失败整体回滚；
// 命中已有 model 则更新价格/备注，保留原 ID 与 created_at。
func (o *Op) UpsertModelPricesBatch(ps []model.ModelPrice) error {
	if len(ps) == 0 {
		return nil
	}
	return o.DB.Transaction(func(tx *gorm.DB) error {
		for i := range ps {
			var existing model.ModelPrice
			err := tx.Where("model = ?", ps[i].Model).First(&existing).Error
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					if err := tx.Create(&ps[i]).Error; err != nil {
						return err
					}
					continue
				}
				return err
			}
			ps[i].ID = existing.ID
			ps[i].CreatedAt = existing.CreatedAt
			if err := tx.Save(&ps[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
