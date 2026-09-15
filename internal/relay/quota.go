package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/op"

	"github.com/redis/go-redis/v9"
)

// quota.go key 级额度硬限额（计费）。
//
// 架构（三层记账）：
//   - Redis INCRBYFLOAT quota:used:{key_id} —— 实时计数（拿到 usage 后立刻累加，转发 goroutine 内）
//   - api_keys.quota_used —— DB 权威值（UsageWriter flush 事务增量维护，与 usage_logs 同事务）
//   - 转发前 GET Redis 检查；miss 从 DB 回填（SETNX，避免覆盖在途增量）；Redis 不可用降级读 DB
//
// 与限流（ratelimit.go fail-open）语义相反：额度是计费拦截不是保护措施，
// Redis 故障必须降级到 DB 而不是放行，否则限额形同虚设。
// DB 读取也失败时放行（鉴权已成功说明 DB 刚才还活着，短暂抖动不应挡死转发面；打日志）。
//
// 超支窗口 = 在途并发请求的费用（请求已发上游无法撤回），单请求几美分量级，可接受。

// quotaUsedKey Redis 实时额度键。
func quotaUsedKey(apiKeyID int64) string {
	return fmt.Sprintf("quota:used:%d", apiKeyID)
}

// quotaStore 额度回填读取接口（*op.Op 满足；测试可注入）。
type quotaStore interface {
	GetApiKeyGlobal(id int64) (*model.ApiKey, error)
}

// QuotaEnforcer key 级额度检查器。
type QuotaEnforcer struct {
	rdb *redis.Client
	op  quotaStore
	// Enabled 平台开关（QUOTA_ENFORCE，默认开）；关闭时全放行（纯统计模式）。
	Enabled bool
}

func NewQuotaEnforcer(rdb *redis.Client, o *op.Op, enabled bool) *QuotaEnforcer {
	return &QuotaEnforcer{rdb: rdb, op: o, Enabled: enabled}
}

// NewQuotaEnforcerWithStore 自定义 store（测试注入用）。
func NewQuotaEnforcerWithStore(rdb *redis.Client, store quotaStore, enabled bool) *QuotaEnforcer {
	return &QuotaEnforcer{rdb: rdb, op: store, Enabled: enabled}
}

// OverLimit 转发前检查：key 是否已超额度。返回 (used, limit, over)。
// limit<=0（不限额）恒不超限；读不到当前值时放行（over=false）。
func (q *QuotaEnforcer) OverLimit(ctx context.Context, apiKey *model.ApiKey) (used, limit float64, over bool) {
	if q == nil || !q.Enabled || apiKey.QuotaLimit <= 0 {
		return 0, apiKey.QuotaLimit, false
	}
	used, err := q.currentUsed(ctx, apiKey)
	if err != nil {
		log.Printf("[quota] read used failed for key %d, allow (fail-open on storage error): %v", apiKey.ID, err)
		return 0, apiKey.QuotaLimit, false
	}
	return used, apiKey.QuotaLimit, used >= apiKey.QuotaLimit
}

// currentUsed 当前已用额度：Redis 实时值 → miss 从 DB 回填（SETNX）→ Redis 故障降级 DB。
func (q *QuotaEnforcer) currentUsed(ctx context.Context, apiKey *model.ApiKey) (float64, error) {
	if q.rdb != nil {
		v, err := q.rdb.Get(ctx, quotaUsedKey(apiKey.ID)).Result()
		if err == nil {
			return strconv.ParseFloat(v, 64)
		}
		if errors.Is(err, redis.Nil) {
			// miss（重启/淘汰/重置后首次）：从 DB 权威值回填。
			// SETNX 防止覆盖并发 goroutine 的在途 INCRBYFLOAT 增量。
			dbUsed, derr := q.dbUsed(apiKey)
			if derr != nil {
				return 0, derr
			}
			if err := q.rdb.SetNX(ctx, quotaUsedKey(apiKey.ID),
				strconv.FormatFloat(dbUsed, 'f', -1, 64), 0).Err(); err != nil {
				return 0, err
			}
			return dbUsed, nil
		}
		// Redis 错误 → 降级 DB（不 fail-open）
	}
	return q.dbUsed(apiKey)
}

// dbUsed DB 权威值（UsageWriter flush 增量维护；滞后 ≤ flush 周期）。
func (q *QuotaEnforcer) dbUsed(apiKey *model.ApiKey) (float64, error) {
	if k, err := q.op.GetApiKeyGlobal(apiKey.ID); err != nil {
		return 0, err
	} else {
		return k.QuotaUsed, nil
	}
}

// Add 请求完成后实时累加费用（recordUsage 内调用）。Redis 不可用静默跳过 ——
// DB 权威值由 UsageWriter flush 维护，Redis 侧丢失只在 miss 后按 DB 回填自愈。
func (q *QuotaEnforcer) Add(ctx context.Context, apiKeyID int64, cost float64) {
	if q == nil || !q.Enabled || cost == 0 || q.rdb == nil {
		return
	}
	if err := q.rdb.IncrByFloat(ctx, quotaUsedKey(apiKeyID), cost).Err(); err != nil {
		log.Printf("[quota] incr failed for key %d: %v", apiKeyID, err)
	}
}

// Reset 重置用量：删 Redis 键（DB 清零由调用方事务完成；下次检查 miss 按 DB 回填 0）。
func (q *QuotaEnforcer) Reset(ctx context.Context, apiKeyID int64) {
	if q == nil || q.rdb == nil {
		return
	}
	if err := q.rdb.Del(ctx, quotaUsedKey(apiKeyID)).Err(); err != nil {
		log.Printf("[quota] reset del key %d: %v", apiKeyID, err)
	}
}

// quotaExceededResponse 402 响应（按入口协议格式化；OpenAI/Anthropic SDK 均原样透传状态码）。
func quotaExceededResponse(w http.ResponseWriter, entry string, used, limit float64) {
	gatewayError(w, entry, http.StatusPaymentRequired,
		fmt.Sprintf("quota exceeded for this api key: used $%.4f of $%.4f limit (reset or raise the limit in dashboard)", used, limit))
}
