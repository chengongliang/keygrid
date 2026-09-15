package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
)

// sync_test.go RefreshProvider / ObserveHeaders —— fake store + httptest 上游
// （同 oidc_settings_test 的接口打桩风格；项目无 DB 测试基建）。

// fakeStore 内存版 Store。
type fakeStore struct {
	mu         sync.Mutex
	creds      map[int64]*model.Credential // providerID → credential
	snapshots  map[int64]*model.QuotaSnapshot
	upserts    int
	proxyURL   string
	proxyError bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		creds:     map[int64]*model.Credential{},
		snapshots: map[int64]*model.QuotaSnapshot{},
	}
}

func (f *fakeStore) GetCredentialByProviderID(providerID int64) (*model.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.creds[providerID]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, op.ErrNotFound
}

func (f *fakeStore) GetQuotaSnapshotGlobal(providerID int64) (*model.QuotaSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.snapshots[providerID]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, op.ErrNotFound
}

func (f *fakeStore) UpsertQuotaSnapshot(s *model.QuotaSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	cp := *s
	f.snapshots[s.ProviderID] = &cp
	return nil
}

func (f *fakeStore) MarkQuotaSnapshotError(providerID, userID int64, platform, msg string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.snapshots[providerID]; ok {
		s.Status = "error"
		s.Error = msg
		return nil
	}
	f.snapshots[providerID] = &model.QuotaSnapshot{
		ProviderID: providerID, UserID: userID, Platform: platform,
		Status: "error", Error: msg, FetchedAt: now,
	}
	return nil
}

func (f *fakeStore) ProxyURL() (string, error) {
	if f.proxyError {
		return "", errors.New("no proxy")
	}
	return f.proxyURL, nil
}

// 加密凭据（与真实 credentials.enc_data 同结构）。
func encryptToken(t *testing.T, accessToken, accountID string) []byte {
	t.Helper()
	plain, _ := json.Marshal(oauth.TokenSetData{
		AccessToken: accessToken,
		Extra:       map[string]string{"chatgptAccountId": accountID},
	})
	enc, err := crypto.Encrypt(plain)
	if err != nil {
		t.Fatalf("crypto.Encrypt: %v", err)
	}
	return enc
}

func init() {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	crypto.Init(k)
}

// 测试上游是 httptest（127.0.0.1）：放行 SSRF 私网拦截（与 e2e 的 SSRF_ALLOW_INTERNAL 同语义）。
// 本包测试不并行，进程级 env 设置安全。
func init() {
	os.Setenv("SSRF_ALLOW_INTERNAL", "1")
}

// ---- httptest 上游 ----

// newUsageUpstream 启一个 /backend-api/wham/usage 的 mock 上游。
// handler 返回 (status, body)；记录收到的 Authorization / account id 头。
func newUsageUpstream(t *testing.T, status int, body string, seen *[][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/wham/usage") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if seen != nil {
			*seen = append(*seen, []string{r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func testProvider(id int64, userID int64, baseURL string, enabled bool) model.Provider {
	return model.Provider{
		ID: id, UserID: userID, Name: fmt.Sprintf("ch-%d", id),
		Kind: "oauth", Protocol: "openai", OAuthProvider: "openai",
		BaseURL: baseURL, Enabled: enabled,
	}
}

// ---- 用例 ----

// TestRefreshProvider_SnapshotShape 手动刷新成功路径：探测 → 快照 ok/probe 落库，
// 快照形状（user/platform/plan/reset 额度）完整。
func TestRefreshProvider_SnapshotShape(t *testing.T) {
	up := newUsageUpstream(t, http.StatusOK, sampleWhamUsage, nil)
	defer up.Close()

	fs := newFakeStore()
	p := testProvider(1, 10, up.URL+"/backend-api/codex/responses", true)
	fs.creds[1] = &model.Credential{ProviderID: 1, Status: "active", EncData: encryptToken(t, "tok-1", "acct-1")}

	s := NewSyncer(fs)
	snap, cached, err := s.RefreshProvider(context.Background(), &p)
	if err != nil || cached {
		t.Fatalf("refresh: cached=%v err=%v", cached, err)
	}
	stored, err := fs.GetQuotaSnapshotGlobal(1)
	if err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if stored.Status != "ok" || stored.Source != "probe" {
		t.Errorf("snapshot status/source = %s/%s, want ok/probe", stored.Status, stored.Source)
	}
	if stored.UserID != 10 || stored.Platform != "openai" {
		t.Errorf("snapshot user/platform = %d/%s", stored.UserID, stored.Platform)
	}
	if stored.Data.PlanType != "pro" || stored.Data.ResetCreditsAvailable == nil || *stored.Data.ResetCreditsAvailable != 2 {
		t.Errorf("snapshot data = %+v", stored.Data)
	}
	_ = snap
}

// TestRefreshProvider_UpstreamError 上游 401：返回错误 + 快照标记 error（保留旧数据语义）。
func TestRefreshProvider_UpstreamError(t *testing.T) {
	up := newUsageUpstream(t, http.StatusUnauthorized, `{"error":"invalid_token"}`, nil)
	defer up.Close()

	fs := newFakeStore()
	p := testProvider(1, 10, up.URL+"/backend-api/codex/responses", true)
	fs.creds[1] = &model.Credential{ProviderID: 1, Status: "active", EncData: encryptToken(t, "tok-1", "")}

	s := NewSyncer(fs)
	if _, _, err := s.RefreshProvider(context.Background(), &p); err == nil {
		t.Fatal("expect upstream error")
	}
	bad, err := fs.GetQuotaSnapshotGlobal(1)
	if err != nil || bad.Status != "error" {
		t.Errorf("snapshot = %+v err=%v, want status=error", bad, err)
	} else if !strings.Contains(bad.Error, "401") {
		t.Errorf("error = %q, want contains 401", bad.Error)
	}
}

// TestRefreshProvider_ProxyRequiredButMissing 渠道要求代理但平台未配置：
// 返回错误（单渠道失败不冒泡到 panic），快照标记 error。
func TestRefreshProvider_ProxyRequiredButMissing(t *testing.T) {
	up := newUsageUpstream(t, http.StatusOK, sampleWhamUsage, nil)
	defer up.Close()

	fs := newFakeStore()
	fs.proxyError = true
	p := testProvider(1, 10, up.URL+"/backend-api/codex/responses", true)
	p.UseProxy = true
	fs.creds[1] = &model.Credential{ProviderID: 1, Status: "active", EncData: encryptToken(t, "tok-1", "")}

	s := NewSyncer(fs)
	if _, _, err := s.RefreshProvider(context.Background(), &p); err == nil {
		t.Fatal("expect proxy error")
	}
	if snap, err := fs.GetQuotaSnapshotGlobal(1); err != nil || snap.Status != "error" {
		t.Errorf("snapshot = %+v err=%v, want error placeholder", snap, err)
	}
}

func TestRefreshProvider_RateLimit(t *testing.T) {
	var mu sync.Mutex
	seen := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sampleWhamUsage))
	}))
	defer up.Close()

	fs := newFakeStore()
	p := testProvider(1, 10, up.URL+"/backend-api/codex/responses", true)
	fs.creds[1] = &model.Credential{ProviderID: 1, Status: "active", EncData: encryptToken(t, "tok-1", "acct-1")}

	s := NewSyncer(fs)
	s.MinInterval = time.Minute
	fixed := time.Now()
	s.Now = func() time.Time { return fixed }

	if _, cached, err := s.RefreshProvider(context.Background(), &p); err != nil || cached {
		t.Fatalf("first refresh: cached=%v err=%v, want real", cached, err)
	}
	snap, cached, err := s.RefreshProvider(context.Background(), &p)
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if !cached {
		t.Errorf("second refresh should hit rate limit window")
	}
	if snap == nil || snap.Data.PlanType != "pro" {
		t.Errorf("cached snapshot = %+v", snap)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 1 {
		t.Errorf("upstream calls = %d, want 1 (rate limited)", seen)
	}
}

func TestObserveHeaders_MergesKeepingProbeFields(t *testing.T) {
	fs := newFakeStore()
	p := testProvider(1, 10, "https://chatgpt.com/backend-api/codex/responses", true)
	// 已有 probe 快照：完整数据（credits / reset credits / additional）
	probeData, err := ParseCodexUsage([]byte(sampleWhamUsage))
	if err != nil {
		t.Fatalf("ParseCodexUsage: %v", err)
	}
	fs.snapshots[1] = &model.QuotaSnapshot{
		ProviderID: 1, UserID: 10, Platform: "openai", Status: "ok",
		Data: *probeData, Source: "probe", FetchedAt: time.Now().Add(-time.Hour),
	}

	s := NewSyncer(fs)
	h := http.Header{}
	h.Set("X-Codex-Plan-Type", "pro")
	h.Set("X-Codex-Primary-Used-Percent", "99")
	h.Set("X-Codex-Primary-Window-Minutes", "10080")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	s.ObserveHeaders(&p, h)

	// 异步落库：轮询等待
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := fs.GetQuotaSnapshotGlobal(1)
		if err == nil && snap.Source == "relay" {
			if snap.Data.RateLimit == nil || snap.Data.RateLimit.Primary.UsedPercent != 99 {
				t.Fatalf("observed rate_limit = %+v", snap.Data.RateLimit)
			}
			// probe 来源字段保留
			if snap.Data.ResetCreditsAvailable == nil || *snap.Data.ResetCreditsAvailable != 2 {
				t.Errorf("reset credits lost: %+v", snap.Data.ResetCreditsAvailable)
			}
			if snap.Data.Credits == nil {
				t.Errorf("credits lost")
			}
			if len(snap.Data.Additional) != 1 {
				t.Errorf("additional lost")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("observe merge not persisted in time")
}

func TestObserveHeaders_CreatesSnapshotWhenAbsent(t *testing.T) {
	fs := newFakeStore()
	p := testProvider(1, 10, "https://chatgpt.com/backend-api/codex/responses", true)
	s := NewSyncer(fs)
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "5")
	s.ObserveHeaders(&p, h)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap, err := fs.GetQuotaSnapshotGlobal(1); err == nil {
			if snap.Status != "ok" || snap.Source != "relay" || snap.Data.RateLimit == nil {
				t.Fatalf("snapshot = %+v", snap)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("observe snapshot not created in time")
}

func TestObserveHeaders_IgnoresNonQuotaHeaders(t *testing.T) {
	fs := newFakeStore()
	p := testProvider(1, 10, "https://chatgpt.com/backend-api/codex/responses", true)
	s := NewSyncer(fs)
	s.ObserveHeaders(&p, http.Header{"Content-Type": {"application/json"}})
	time.Sleep(50 * time.Millisecond)
	if _, err := fs.GetQuotaSnapshotGlobal(1); !errors.Is(err, op.ErrNotFound) {
		t.Errorf("non-quota headers should not create snapshot, err=%v", err)
	}
}
