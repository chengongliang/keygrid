package op

import (
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

// reqerr.go 失败请求诊断（request_errors）的数据访问：写入 / 查询 / 过期清理。
//
// 与 usage_logs 分开存的原因见 model/request_error.go 注释；读写都在这里统一
// 收口，避免 handler / relay 直接写表。

// RequestErrorRow 失败详情列表行（JOIN 出渠道名与 key 名，前端直接展示）。
type RequestErrorRow struct {
	ID           int64     `json:"id"`
	ProviderID   int64     `json:"provider_id"`
	ProviderName string    `json:"provider_name"`
	APIKeyName   string    `json:"api_key_name"`
	Model        string    `json:"model"`
	Kind         string    `json:"kind"`
	StatusCode   int       `json:"status_code"`
	Message      string    `json:"message"`
	LatencyMs    int       `json:"latency_ms"`
	CreatedAt    time.Time `json:"created_at"`
}

// InsertRequestErrors 批量落库失败诊断（relay 的 ErrorWriter 调用）。
func (o *Op) InsertRequestErrors(rows []*model.RequestError) error {
	if len(rows) == 0 {
		return nil
	}
	return o.DB.CreateInBatches(rows, 100).Error
}

// ListRequestErrors 按渠道或用户维度查失败详情（时间倒序）。
//
//   - userID > 0：只返回该用户的记录（用户端强制传自己的 id，数据隔离硬性规则）；
//   - providerID > 0：只看指定渠道；providerID = 0 时含路由层失败（provider_id=0）；
//   - includeRouting：连路由层失败（provider_id=0）一起返回。这些记录不属于任何渠道，
//     但正是「请求为什么失败」的关键信息（无渠道支持该模型 / 渠道全被禁用）。
//     用户端渠道详情开启它；admin 渠道详情不开（否则每条渠道都会带上全平台的
//     路由层失败，噪音压过本渠道的问题）。
//
// 两种过滤都为空时返回空（避免无过滤的全表扫描被误用）。
func (o *Op) ListRequestErrors(userID, providerID int64, includeRouting bool, limit int) ([]RequestErrorRow, error) {
	if userID <= 0 && providerID <= 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := o.DB.Table("request_errors e").
		Select("e.id as id, e.provider_id as provider_id, COALESCE(p.name,'') as provider_name, " +
			"COALESCE(k.name,'') as api_key_name, e.model as model, e.kind as kind, " +
			"e.status_code as status_code, e.message as message, e.latency_ms as latency_ms, e.created_at as created_at").
		Joins("LEFT JOIN providers p ON p.id = e.provider_id").
		Joins("LEFT JOIN api_keys k ON k.id = e.api_key_id")
	if userID > 0 {
		q = q.Where("e.user_id = ?", userID)
	}
	if providerID > 0 {
		if includeRouting {
			q = q.Where("e.provider_id = ? OR e.provider_id = 0", providerID)
		} else {
			q = q.Where("e.provider_id = ?", providerID)
		}
	}
	var rows []RequestErrorRow
	if err := q.Order("e.id DESC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CleanupRequestErrors 删除过期失败诊断（后台任务按保留期调用）。
func (o *Op) CleanupRequestErrors(before time.Time) (int64, error) {
	res := o.DB.Where("created_at < ?", before).Delete(&model.RequestError{})
	return res.RowsAffected, res.Error
}
