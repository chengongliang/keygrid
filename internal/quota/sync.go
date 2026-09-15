package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/ssrf"
)

// sync.go 额度同步器。两个入口共用一套快照存储：
//   - RefreshProvider  用户手动刷新（handler；带最小间隔软限频，防高频轮询上游）
//   - ObserveHeaders   relay 被动观察（转发响应 x-codex-* 头，异步合并不阻塞热路径）
//
// 注：已移除后台周期主动探测（原 SyncAll，经 task.Runner 轮询）—— 不对上游
// /wham/usage 做周期性主动轮询；额度更新只发生在用户手动刷新与真实转发的响应头上。

// Store 同步器对存储层的依赖（*op.Op 实现；接口化便于单测打桩）。
type Store interface {
	GetCredentialByProviderID(providerID int64) (*model.Credential, error)
	GetQuotaSnapshotGlobal(providerID int64) (*model.QuotaSnapshot, error)
	UpsertQuotaSnapshot(s *model.QuotaSnapshot) error
	MarkQuotaSnapshotError(providerID, userID int64, platform, msg string, now time.Time) error
	ProxyURL() (string, error)
}

// Syncer 额度同步器（无共享状态依赖；同进程内手动刷新限频用内存 map）。
type Syncer struct {
	Op Store
	// MinInterval 手动刷新最小间隔（同渠道两次真实上游查询之间）；0 = 30s。
	MinInterval time.Duration
	// Now 测试注入。
	Now func() time.Time

	mu          sync.Mutex
	lastAttempt map[int64]time.Time // providerID → 上次手动刷新发起时间
}

// NewSyncer 构造。
func NewSyncer(o Store) *Syncer {
	return &Syncer{Op: o, lastAttempt: map[int64]time.Time{}}
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// RefreshProvider 手动刷新单渠道（用户侧 handler 调用；provider 已由 handler 完成
// user 隔离校验）。MinInterval 内重复请求直接返回现有快照（cached=true，不打上游）。
func (s *Syncer) RefreshProvider(ctx context.Context, p *model.Provider) (snap *model.QuotaSnapshot, cached bool, err error) {
	min := s.MinInterval
	if min <= 0 {
		min = 30 * time.Second
	}
	s.mu.Lock()
	if last, ok := s.lastAttempt[p.ID]; ok && s.now().Sub(last) < min {
		s.mu.Unlock()
		snap, gerr := s.Op.GetQuotaSnapshotGlobal(p.ID)
		return snap, true, gerr
	}
	s.lastAttempt[p.ID] = s.now()
	s.mu.Unlock()

	snap, err = s.syncOne(ctx, p)
	return snap, false, err
}

// ObserveHeaders relay 被动观察入口：响应携带 x-codex-* 头时异步合并进快照，
// 不阻塞转发热路径；内部错误仅记日志。
func (s *Syncer) ObserveHeaders(p *model.Provider, h http.Header) {
	data := ParseCodexHeaders(h)
	if data == nil || p == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[quota] provider %d observe panic: %v", p.ID, r)
			}
		}()
		if err := s.mergeObservation(p, data); err != nil {
			log.Printf("[quota] provider %d observe merge: %v", p.ID, err)
		}
	}()
}

// mergeObservation 被动观察落库（读-合并-写）：
// 替换 rate_limit/plan_type（响应头自带），保留主动探测的 credits/reset_credits/
// additional（响应头里没有）。并发下偶发互相覆盖可接受 —— 下次主动探测补全。
func (s *Syncer) mergeObservation(p *model.Provider, obs *model.QuotaData) error {
	data := *obs
	if old, err := s.Op.GetQuotaSnapshotGlobal(p.ID); err == nil {
		data.Credits = old.Data.Credits
		data.ResetCreditsAvailable = old.Data.ResetCreditsAvailable
		data.Additional = old.Data.Additional
	} else if !errors.Is(err, op.ErrNotFound) {
		return err
	}
	return s.Op.UpsertQuotaSnapshot(&model.QuotaSnapshot{
		ProviderID: p.ID,
		UserID:     p.UserID,
		Platform:   p.OAuthProvider,
		Status:     "ok",
		Data:       data,
		Source:     "relay",
		FetchedAt:  s.now(),
	})
}

// syncOne 探测单个渠道并落库（手动刷新路径）：解密凭据 → 派生 usage URL（SSRF
// 校验）→ GET /wham/usage → 解析 → upsert；失败写 error 快照（保留旧数据）。
func (s *Syncer) syncOne(ctx context.Context, p *model.Provider) (*model.QuotaSnapshot, error) {
	data, err := s.probe(ctx, p)
	if err != nil {
		// 失败写 error 快照：已有快照时保留 data（前端继续展示上次成功数据 + 错误提示）
		if merr := s.Op.MarkQuotaSnapshotError(p.ID, p.UserID, p.OAuthProvider, err.Error(), s.now()); merr != nil {
			return nil, merr
		}
		return nil, err
	}
	snap := &model.QuotaSnapshot{
		ProviderID: p.ID,
		UserID:     p.UserID,
		Platform:   p.OAuthProvider,
		Status:     "ok",
		Data:       *data,
		Source:     "probe",
		FetchedAt:  s.now(),
	}
	if err := s.Op.UpsertQuotaSnapshot(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// probe 单渠道探测：解密凭据 → 派生 usage URL（SSRF 校验）→ GET /wham/usage → 解析。
func (s *Syncer) probe(ctx context.Context, p *model.Provider) (*model.QuotaData, error) {
	cred, err := s.Op.GetCredentialByProviderID(p.ID)
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
	}
	tok, err := decodeTokenSet(cred.EncData)
	if err != nil {
		return nil, err
	}
	usageURL := CodexUsageURL(p.BaseURL)
	// usage URL 从用户可控的 base_url 派生，必须过 SSRF 校验（同渠道创建防私信网探测）
	if err := ssrf.CheckBaseURL(usageURL); err != nil {
		return nil, fmt.Errorf("usage url %s: %w", usageURL, err)
	}
	client, err := s.clientFor(p)
	if err != nil {
		return nil, err
	}
	return FetchCodexUsage(ctx, client, usageURL, tok.AccessToken, tok.Extra["chatgptAccountId"])
}

// clientFor 渠道代理感知的 HTTP client（与 relay/oauth 刷新同策略：勾选代理走
// 平台 proxy_url，未配置报错；未勾选直连）。
func (s *Syncer) clientFor(p *model.Provider) (*http.Client, error) {
	proxyURL := ""
	if p.UseProxy {
		v, err := s.Op.ProxyURL()
		if err != nil {
			return nil, fmt.Errorf("provider %d: read proxy setting: %w", p.ID, err)
		}
		if v == "" {
			return nil, fmt.Errorf("provider %d (%s) requires proxy, but platform proxy is not configured", p.ID, p.OAuthProvider)
		}
		proxyURL = v
	}
	return httpx.Client(codexUsageTimeout, proxyURL)
}

// decodeTokenSet 解密 oauth 凭据为 TokenSetData（api_key 凭据无 usage 数据可查）。
func decodeTokenSet(enc []byte) (*oauth.TokenSetData, error) {
	plain, err := crypto.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential: %w", err)
	}
	var d oauth.TokenSetData
	if err := json.Unmarshal(plain, &d); err != nil {
		return nil, fmt.Errorf("credential data: %w", err)
	}
	if d.AccessToken == "" {
		return nil, errors.New("credential has no oauth access token")
	}
	return &d, nil
}
