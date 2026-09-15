package relay

import (
	"log"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
)

// usage.go usage_logs 异步批量写入。
// 转发路径只往 channel 丢一条记录，由后台 goroutine 攒批落库（默认 100 条或 2s）。
// flush 事务里同时按 key 增量累计 api_keys.quota_used（DB 权威值）；
// Redis 实时额度计数由调用方在拿到 usage 后立刻完成（quota.go）。

type usageEntry struct {
	userID           int64
	apiKeyID         int64
	providerID       int64
	model            string
	billingModel     string // 计费归一化名（渠道 BillingMap；空 = 同 model）
	promptTokens     int
	completionTokens int
	statusCode       int
	latencyMs        int
	cost             float64 // USD（请求时价格快照；未定价模型 0）
}

// UsageStore 落库接口（*op.Op 满足）。
type UsageStore interface {
	FlushUsageBatch(logs []*model.UsageLog, quotaDeltas map[int64]float64) error
}

// UsageWriter 异步批量记账器。
type UsageWriter struct {
	ch     chan usageEntry
	store  UsageStore
	batch  int
	period time.Duration

	stopOnce sync.Once
	done     chan struct{}
}

func NewUsageWriter(store UsageStore, batchSize int, flushPeriod time.Duration) *UsageWriter {
	if batchSize <= 0 {
		batchSize = 100
	}
	if flushPeriod <= 0 {
		flushPeriod = 2 * time.Second
	}
	uw := &UsageWriter{
		ch:     make(chan usageEntry, 4096),
		store:  store,
		batch:  batchSize,
		period: flushPeriod,
		done:   make(chan struct{}),
	}
	go uw.run()
	return uw
}

// Record 异步投递一条记账（永不阻塞转发路径；队列满则丢弃并打日志）。
func (uw *UsageWriter) Record(e usageEntry) {
	select {
	case uw.ch <- e:
	default:
		log.Printf("[usage] queue full, drop record user=%d model=%s", e.userID, e.model)
	}
}

func (uw *UsageWriter) run() {
	defer close(uw.done)
	ticker := time.NewTicker(uw.period)
	defer ticker.Stop()
	batch := make([]*model.UsageLog, 0, uw.batch)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// 按 key 聚合费用增量（同批可能有多条同一 key 的记录）
		deltas := map[int64]float64{}
		for _, ul := range batch {
			if ul.Cost != 0 {
				deltas[ul.ApiKeyID] += ul.Cost
			}
		}
		if err := uw.store.FlushUsageBatch(batch, deltas); err != nil {
			log.Printf("[usage] batch flush failed (%d records): %v", len(batch), err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-uw.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, &model.UsageLog{
				UserID:           e.userID,
				ApiKeyID:         e.apiKeyID,
				ProviderID:       e.providerID,
				Model:            e.model,
				BillingModel:     e.billingModel,
				PromptTokens:     e.promptTokens,
				CompletionTokens: e.completionTokens,
				StatusCode:       e.statusCode,
				LatencyMs:        e.latencyMs,
				Cost:             e.cost,
			})
			if len(batch) >= uw.batch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Stop 落库剩余记录后退出（进程关闭时调用）。
func (uw *UsageWriter) Stop() {
	uw.stopOnce.Do(func() {
		close(uw.ch)
		<-uw.done
	})
}
