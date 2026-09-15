package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// ---- iFlow：authorization_code + Basic Auth ----

func TestIFlowBeginAuthURL(t *testing.T) {
	res, _, err := (IFlow{}).BeginAuth(t.Context(), oauth.Callbacks{PublicBaseURL: "https://gw.example.com"}, "st-1")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	for _, want := range []string{
		"https://iflow.cn/oauth?", "loginMethod=phone", "type=phone",
		"client_id=" + iflowClientID, "state=st-1",
		"redirect=https%3A%2F%2Fgw.example.com%2Fapi%2Foauth%2Fcallback",
	} {
		if !strings.Contains(res.AuthorizeURL, want) {
			t.Fatalf("authorize url missing %q: %s", want, res.AuthorizeURL)
		}
	}
}

func TestIFlowResolveAndRefresh(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Basic "+iflowBasicAuth() {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"bad basic"}`))
			return
		}
		if r.Form.Get("grant_type") == "authorization_code" {
			if r.Form.Get("code") != "ac-1" || r.Form.Get("client_secret") != iflowClientSecret {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-if", "refresh_token": "rt-if", "expires_in": 3600,
			})
			return
		}
		// refresh
		if r.Form.Get("refresh_token") != "rt-if" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-if2", "expires_in": 1800,
		})
	}))
	defer up.Close()

	orig := iflowTokenURL
	defer func() { iflowTokenURL = orig }()
	iflowTokenURL = up.URL

	ifl := IFlow{}
	tok, err := ifl.Resolve(t.Context(), oauth.Callbacks{}, map[string]string{"code": "ac-1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.AccessToken != "at-if" || tok.RefreshToken != "rt-if" {
		t.Fatalf("token: %+v", tok)
	}

	// 回调错误透传
	if _, err := ifl.Resolve(t.Context(), oauth.Callbacks{}, map[string]string{
		oauth.CallbackParamError: "access_denied",
	}); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("expected ResponseAuthError, got %v", err)
	}

	newTok, err := ifl.Refresh(t.Context(), oauth.Callbacks{}, tok)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newTok.AccessToken != "at-if2" {
		t.Fatalf("refresh token: %+v", newTok)
	}
	// refresh_token 未轮换 → 保留旧值
	if newTok.RefreshToken != "rt-if" {
		t.Fatalf("refresh fallback: %q", newTok.RefreshToken)
	}
	_ = gotAuth
}

// ---- Qoder：本地 PKCE+nonce+machineId → deviceToken/poll 轮询 ----

func TestQoderBeginAndPoll(t *testing.T) {
	var pollCount int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/deviceToken/poll") {
			w.WriteHeader(404)
			return
		}
		q := r.URL.Query()
		if q.Get("nonce") == "" || q.Get("verifier") == "" || q.Get("challenge_method") != "S256" {
			w.WriteHeader(400)
			return
		}
		pollCount++
		if pollCount < 2 {
			w.WriteHeader(202) // pending
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "dt-qoder",
			"expires_at": float64(time.Now().Add(24 * time.Hour).UnixMilli()),
			"user_id":    "u-7",
			"needs_mill": true,
		})
	}))
	defer up.Close()

	orig := qoderDeviceTokenURL
	defer func() { qoderDeviceTokenURL = orig }()
	qoderDeviceTokenURL = up.URL + "/api/v1/deviceToken/poll"

	qd := Qoder{}
	begin, temp, err := qd.BeginAuth(t.Context(), oauth.Callbacks{}, "")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	if !strings.Contains(begin.VerificationURIComp, "qoder.com/device") ||
		!strings.Contains(begin.VerificationURIComp, "challenge=") ||
		!strings.Contains(begin.VerificationURIComp, "machine_id=") {
		t.Fatalf("login url shape: %s", begin.VerificationURIComp)
	}
	if temp["nonce"] == "" || temp["code_verifier"] == "" || temp["machine_id"] == "" {
		t.Fatalf("temp must carry nonce/verifier/machineId: %v", temp)
	}

	// 第一次 poll 202 → pending
	if _, err := qd.Resolve(t.Context(), oauth.Callbacks{}, temp); err == nil {
		t.Fatal("expected pending on 202")
	} else if !strings.Contains(err.Error(), "pending") && !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending err: %v", err)
	}
	tok, err := qd.Resolve(t.Context(), oauth.Callbacks{}, temp)
	if err != nil {
		t.Fatalf("final poll: %v", err)
	}
	if tok.AccessToken != "dt-qoder" {
		t.Fatalf("token: %+v", tok)
	}
	if tok.Extra["userId"] != "u-7" || tok.Extra["machineId"] == "" {
		t.Fatalf("extra: %+v", tok.Extra)
	}
	if tok.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expires_at parse failed: %v", tok.ExpiresAt)
	}
}

func TestQoderParseExpiryVariants(t *testing.T) {
	now := time.Unix(1788000000, 0)
	// ms epoch 数字
	if got := qoderParseExpiry(float64(1789000000000), nil, now); !got.Equal(time.UnixMilli(1789000000000)) {
		t.Fatalf("numeric ms: %v", got)
	}
	// 数字字符串 ms epoch
	if got := qoderParseExpiry("1789000000000", nil, now); !got.Equal(time.UnixMilli(1789000000000)) {
		t.Fatalf("string ms: %v", got)
	}
	// RFC3339
	if got := qoderParseExpiry("2026-06-16T07:15:04Z", nil, now); got.IsZero() {
		t.Fatalf("rfc3339: %v", got)
	}
	// expires_in 秒
	if got := qoderParseExpiry(nil, float64(60), now); !got.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("expires_in: %v", got)
	}
	// 全无 → 兜底 30 天
	if got := qoderParseExpiry(nil, nil, now); !got.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("fallback 30d: %v", got)
	}
}

// ---- Trae：GetLoginGuidance → ExchangeToken ----

func TestTraeBeginAndResolve(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/GetLoginGuidance":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["loginTraceID"] == "" {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{"LoginHost": "api.marscode.com"},
			})
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["ClientID"] != traeClientID || body["RefreshToken"] != "rt-trae" || body["ClientSecret"] != "-" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"message":"bad exchange"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{
					"AccessToken":  "at-trae",
					"RefreshToken": "rt-trae-2",
					"ExpiresAt":    "1789000000", // 秒 epoch
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	origOverride := traeEnvOverride
	defer func() { traeEnvOverride = origOverride }()
	traeEnvOverride = up.URL + "/cloudide/api/v3/trae/GetLoginGuidance" // override 为完整端点

	// ExchangeToken 多 origin：把白名单也劫持到 mock（只能整体替换 slice）
	origOrigins := traeAPIOrigins
	defer func() { traeAPIOrigins = origOrigins }()
	traeAPIOrigins = []string{up.URL}

	tr := Trae{}
	begin, temp, err := tr.BeginAuth(t.Context(), oauth.Callbacks{PublicBaseURL: "https://gw.example.com"}, "trace-1")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	if !strings.Contains(begin.VerificationURI, "authorization") ||
		!strings.Contains(begin.VerificationURI, "client_id=") ||
		!strings.Contains(begin.VerificationURI, "login_trace_id=trace-1") ||
		!strings.Contains(begin.VerificationURI, "auth_callback_url=") {
		t.Fatalf("verification url shape: %s", begin.VerificationURI)
	}

	// 回调：refreshToken=rt-trae&loginHost=...&isRedirect=true
	raw := url.Values{
		"isRedirect":   {"true"},
		"refreshToken": {"rt-trae"},
		"loginHost":    {"evil.example.com"}, // 故意不采信（SSRF guard）
	}
	temp[oauth.CallbackParamRaw] = raw.Encode()
	tok, err := tr.Resolve(t.Context(), oauth.Callbacks{}, temp)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.AccessToken != "at-trae" || tok.RefreshToken != "rt-trae-2" {
		t.Fatalf("token: %+v", tok)
	}
	if tok.Extra["authMethod"] != "oauth" {
		t.Fatalf("authMethod: %+v", tok.Extra)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatalf("expires_at from ExpiresAt expected")
	}

	// 粘贴 token 模式：裸 Cloud-IDE-JWT
	tok2, err := tr.Resolve(t.Context(), oauth.Callbacks{}, map[string]string{
		oauth.CallbackParamRaw: "Cloud-IDE-JWT abc.def.ghi",
	})
	if err != nil {
		t.Fatalf("paste-token resolve: %v", err)
	}
	if tok2.AccessToken != "abc.def.ghi" || tok2.Extra["authMethod"] != "imported" {
		t.Fatalf("paste token: %+v", tok2)
	}
}

// ---- CodeBuddy CN：state+authUrl 浏览器轮询流 ----

func TestCodeBuddyBeginAndPoll(t *testing.T) {
	var pollCount int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			if r.Header.Get("X-Domain") != "copilot.tencent.com" || r.Header.Get("X-Product") != "SaaS" {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]string{"state": "cb-state-1", "authUrl": "https://copilot.tencent.com/auth?x=1"},
			})
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token") && r.URL.Query().Get("state") != "":
			pollCount++
			if pollCount < 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 11217, "msg": "RetryFetchToken"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"accessToken":  "at-cb",
					"refreshToken": "rt-cb",
					"expiresIn":    86400,
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	origState, origToken := codebuddyStateURL, codebuddyTokenURL
	defer func() { codebuddyStateURL, codebuddyTokenURL = origState, origToken }()
	codebuddyStateURL = up.URL + "/v2/plugin/auth/state"
	codebuddyTokenURL = up.URL + "/v2/plugin/auth/token"

	cbd := CodeBuddyCN{}
	begin, temp, err := cbd.BeginAuth(t.Context(), oauth.Callbacks{}, "")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	if temp["device_code"] != "cb-state-1" {
		t.Fatalf("temp: %v", temp)
	}
	if begin.VerificationURI != "https://copilot.tencent.com/auth?x=1" {
		t.Fatalf("authUrl: %s", begin.VerificationURI)
	}

	// 第一次 poll 11217 → pending
	if _, err := cbd.Resolve(t.Context(), oauth.Callbacks{}, temp); err == nil {
		t.Fatal("expected pending on 11217")
	}
	tok, err := cbd.Resolve(t.Context(), oauth.Callbacks{}, temp)
	if err != nil {
		t.Fatalf("final poll: %v", err)
	}
	if tok.AccessToken != "at-cb" || tok.RefreshToken != "rt-cb" {
		t.Fatalf("token: %+v", tok)
	}
	if tok.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expires: %v", tok.ExpiresAt)
	}
}

func TestCodeBuddyRefreshHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Refresh-Token") != "rt-old" {
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("X-Auth-Refresh-Source") != "plugin" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"accessToken": "at-cb2", "refreshToken": "rt-new", "expiresIn": 3600},
		})
	}))
	defer up.Close()

	orig := codebuddyRefreshURL
	defer func() { codebuddyRefreshURL = orig }()
	codebuddyRefreshURL = up.URL

	cbd := CodeBuddyCN{}
	newTok, err := cbd.Refresh(t.Context(), oauth.Callbacks{}, &oauth.TokenSet{
		AccessToken: "at-old", RefreshToken: "rt-old",
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newTok.AccessToken != "at-cb2" || newTok.RefreshToken != "rt-new" {
		t.Fatalf("refreshed: %+v", newTok)
	}
}

// ---- 注册表完整性 ----

func TestAdaptersRegistered(t *testing.T) {
	for _, key := range []string{"iflow", "qoder", "trae", "codebuddy-cn"} {
		a, ok := oauth.Lookup(key)
		if !ok {
			t.Fatalf("adapter %q not registered", key)
		}
		if a.Key() != key {
			t.Fatalf("key mismatch: %q != %q", a.Key(), key)
		}
	}
	// kimi 等旧适配器仍在
	if !oauth.IsRegistered("kimi") || !oauth.IsRegistered("openai") || !oauth.IsRegistered("anthropic") {
		t.Fatal("base adapters must remain registered")
	}
}
