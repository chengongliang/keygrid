package model

import (
	"strconv"
	"strings"
	"time"
)

type ApiKey struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	UserID      int64  `gorm:"index;not null" json:"user_id"`
	Name        string `json:"name"`
	KeyHash     string `gorm:"uniqueIndex;not null" json:"-"`
	KeyEnc      []byte `json:"-"`            // AES-GCM 加密的 key 明文（支持"查看/复制"；MasterKey 丢失则不可恢复）
	Prefix      string `json:"prefix"`       // sk-xxxx 前缀展示用
	IPWhitelist string `json:"ip_whitelist"` // 逗号分隔 IP/CIDR，空 = 不限
	ModelLimit  string `json:"model_limit"`  // 逗号分隔模型名，空 = 不限
	// ProviderLimit 逗号分隔渠道 ID 白名单，空 = 不限；非空时该 key 的请求只路由到
	// 白名单内渠道（渠道 ID 不受改名影响；渠道被删除/禁用时绑定自然失效）
	ProviderLimit string `json:"provider_limit"`
	// 额度（落库链路 / 硬拦截）：
	// QuotaLimit 上限 USD；0 = 不限。QuotaUsed 累计已消耗 USD，只由系统（UsageWriter flush）
	// 异步增量维护，接口不可直接改；提供"重置用量"操作清零。
	QuotaLimit float64    `gorm:"default:0" json:"quota_limit"`
	QuotaUsed  float64    `gorm:"default:0" json:"quota_used"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Enabled    bool       `gorm:"default:true" json:"enabled"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (ApiKey) TableName() string { return "api_keys" }

// ProviderIDs 解析 ProviderLimit 为渠道 ID 列表；空串返回 nil（表示不限），
// 非法项（非数字/非正数）与重复项忽略。
func (k *ApiKey) ProviderIDs() []int64 {
	if k == nil || strings.TrimSpace(k.ProviderLimit) == "" {
		return nil
	}
	var ids []int64
	seen := make(map[int64]bool)
	for _, part := range strings.Split(k.ProviderLimit, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
