package providers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// workbuddyTestJWT 造一个不验签的 JWT（header.payload.sig）。
// padded=true 时载荷使用带 padding 的标准 base64url。
func workbuddyTestJWT(t *testing.T, uid string, exp int64, padded bool) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(map[string]any{"uid": uid, "exp": exp})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var payload string
	if padded {
		payload = base64.URLEncoding.EncodeToString(payloadBytes) // 带 padding
	} else {
		payload = base64.RawURLEncoding.EncodeToString(payloadBytes) // 无 padding
	}
	return header + "." + payload + ".sig"
}

func TestWorkBuddyKeyAndFlow(t *testing.T) {
	wb := WorkBuddyIntl{}
	if wb.Key() != "workbuddy-intl" {
		t.Fatalf("Key() = %q", wb.Key())
	}
	if wb.Flow() != oauth.FlowDeviceCode {
		t.Fatalf("Flow() = %q", wb.Flow())
	}
	if !oauth.IsRegistered("workbuddy-intl") {
		t.Fatal("workbuddy-intl not registered")
	}
}

func TestWorkBuddyBeginAndPoll(t *testing.T) {
	access := workbuddyTestJWT(t, "uid-wb-123456789", time.Now().Add(24*time.Hour).Unix(), false)
	var pollCount int
	var pollHeaders http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/plugin/auth/state" && r.Method == http.MethodPost:
			// 授权面请求头与 ?platform=CLI 必须齐全，否则上游直接 400
			// （对照 wb_accounts.py:1203 start_login：url 带 platform，头用 billing_ua）。
			if r.URL.Query().Get("platform") != "CLI" ||
				r.Header.Get("Origin") != "https://www.workbuddy.ai" ||
				r.Header.Get("X-Requested-With") != "XMLHttpRequest" ||
				r.Header.Get("Accept") != "application/json, text/plain, */*" ||
				!strings.Contains(r.Header.Get("User-Agent"), "WorkBuddy/5.5.2") {
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]string{
					"state":   "wb-state-1",
					"authUrl": "https://www.workbuddy.ai/auth?state=wb-state-1",
				},
			})
		case r.URL.Path == "/v2/plugin/auth/token" && r.URL.Query().Get("state") != "":
			pollHeaders = r.Header.Clone()
			pollCount++
			if pollCount < 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 11217, "msg": "RetryFetchToken"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"accessToken":  access,
					"refreshToken": "rt-wb",
					"expiresIn":    86400,
					"domain":       "workbuddy.ai",
				},
			})
		case r.URL.Path == "/v2/plugin/login/account":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]string{"nickname": "国际版用户"},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	origState, origToken, origAccount := workbuddyStateURL, workbuddyTokenURL, workbuddyAccountURL
	defer func() {
		workbuddyStateURL, workbuddyTokenURL, workbuddyAccountURL = origState, origToken, origAccount
	}()
	workbuddyStateURL = up.URL + "/v2/plugin/auth/state"
	workbuddyTokenURL = up.URL + "/v2/plugin/auth/token"
	workbuddyAccountURL = up.URL + "/v2/plugin/login/account"

	wb := WorkBuddyIntl{}
	begin, temp, err := wb.BeginAuth(t.Context(), oauth.Callbacks{}, "")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	if temp["device_code"] != "wb-state-1" {
		t.Fatalf("temp: %v", temp)
	}
	if !strings.Contains(begin.VerificationURI, "wb-state-1") {
		t.Fatalf("begin url must contain state: %s", begin.VerificationURI)
	}

	// 第一次 poll 11217 → pending，可重试
	_, err = wb.Resolve(t.Context(), oauth.Callbacks{}, temp)
	var pe *oauth.PendingError
	if err == nil {
		t.Fatal("expected pending on 11217")
	}
	if !asPending(err, &pe) {
		t.Fatalf("expected *oauth.PendingError, got %T: %v", err, err)
	}

	tok, err := wb.Resolve(t.Context(), oauth.Callbacks{}, temp)
	if err != nil {
		t.Fatalf("final poll: %v", err)
	}
	if tok.AccessToken != access || tok.RefreshToken != "rt-wb" {
		t.Fatalf("token: access=%q refresh=%q", tok.AccessToken, tok.RefreshToken)
	}
	if tok.Extra["uid"] != "uid-wb-123456789" {
		t.Fatalf("uid extra: %+v", tok.Extra)
	}
	if tok.Extra["nickname"] != "国际版用户" {
		t.Fatalf("nickname extra: %+v", tok.Extra)
	}
	if tok.Extra["realm"] != "intl" {
		t.Fatalf("realm extra: %+v", tok.Extra)
	}
	if tok.ExpiresAt.IsZero() || !tok.ExpiresAt.After(time.Now()) {
		t.Fatalf("expiry: %v", tok.ExpiresAt)
	}
	// token 轮询用授权面头部（billing UA + 全 Accept）；参考实现在这一步
	// **不带** X-CodeBuddy-Request（wb_accounts.py:1239-1247），故这里断言的是
	// 授权面那组头，而不是 chat 面的身份头。
	if ua := pollHeaders.Get("User-Agent"); !strings.Contains(ua, "WorkBuddy/5.5.2") {
		t.Fatalf("poll UA = %q, want billing UA", ua)
	}
	if acc := pollHeaders.Get("Accept"); acc != "application/json, text/plain, */*" {
		t.Fatalf("poll Accept = %q", acc)
	}
	if pollHeaders.Get("X-CodeBuddy-Request") != "" {
		t.Fatalf("poll 不应带 chat 面身份头: %v", pollHeaders)
	}
}

func TestWorkBuddyPollTerminalError(t *testing.T) {
	secret := "at-should-not-leak"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 11128,
			"msg":  "token revoked",
			"data": map[string]any{"accessToken": secret},
		})
	}))
	defer up.Close()

	orig := workbuddyTokenURL
	defer func() { workbuddyTokenURL = orig }()
	workbuddyTokenURL = up.URL

	_, err := (WorkBuddyIntl{}).Resolve(t.Context(), oauth.Callbacks{}, map[string]string{"device_code": "s"})
	if err == nil {
		t.Fatal("expected terminal error")
	}
	if !strings.Contains(err.Error(), "11128") {
		t.Fatalf("error must mention code: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must not contain token: %v", err)
	}
	// 终态错误不是 pending
	var pe *oauth.PendingError
	if asPending(err, &pe) {
		t.Fatalf("terminal error must not be pending: %v", err)
	}
}

func TestWorkBuddyRefreshRotatesToken(t *testing.T) {
	var gotRefresh, gotSource, gotDomain string
	var refreshExtra http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshExtra = r.Header.Clone()
		gotRefresh = r.Header.Get("X-Refresh-Token")
		gotSource = r.Header.Get("X-Auth-Refresh-Source")
		gotDomain = r.Header.Get("X-Domain")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"accessToken": "at-wb2", "refreshToken": "rt-wb-new", "expiresIn": 3600},
		})
	}))
	defer up.Close()

	orig := workbuddyRefreshURL
	defer func() { workbuddyRefreshURL = orig }()
	workbuddyRefreshURL = up.URL

	newTok, err := (WorkBuddyIntl{}).Refresh(t.Context(), oauth.Callbacks{}, &oauth.TokenSet{
		AccessToken: "at-old", RefreshToken: "rt-old",
		Extra: map[string]string{"nickname": "n", "domain": "www.workbuddy.ai"},
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotRefresh != "rt-old" {
		t.Fatalf("X-Refresh-Token = %q", gotRefresh)
	}
	if gotSource != "plugin" {
		t.Fatalf("X-Auth-Refresh-Source = %q", gotSource)
	}
	if gotDomain != "www.workbuddy.ai" {
		t.Fatalf("X-Domain = %q", gotDomain)
	}
	// 刷新面必须带 X-CodeBuddy-Request 与 Accept-Language（wb_accounts.py:544-553）。
	if gotReq := refreshExtra.Get("X-CodeBuddy-Request"); gotReq != "1" {
		t.Fatalf("X-CodeBuddy-Request = %q", gotReq)
	}
	if gotLang := refreshExtra.Get("Accept-Language"); gotLang != "en-US" {
		t.Fatalf("Accept-Language = %q", gotLang)
	}
	if newTok.AccessToken != "at-wb2" || newTok.RefreshToken != "rt-wb-new" {
		t.Fatalf("rotated token: %+v", newTok)
	}
	if newTok.ExpiresAt.IsZero() {
		t.Fatal("expiry must be set from expiresIn")
	}
}

func TestWorkBuddyRefreshKeepsOldTokenWhenAbsent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"accessToken": "at-wb3", "expiresIn": 60},
		})
	}))
	defer up.Close()

	orig := workbuddyRefreshURL
	defer func() { workbuddyRefreshURL = orig }()
	workbuddyRefreshURL = up.URL

	newTok, err := (WorkBuddyIntl{}).Refresh(t.Context(), oauth.Callbacks{}, &oauth.TokenSet{
		AccessToken: "at-old", RefreshToken: "rt-keep",
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newTok.RefreshToken != "rt-keep" {
		t.Fatalf("refresh fallback: %q", newTok.RefreshToken)
	}
}

func TestWorkBuddyJWTPayloadDecoding(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	// 无 padding
	unpadded := workbuddyTestJWT(t, "uid-abc", exp, false)
	if c := parseWorkbuddyJWTClaims(unpadded); c.UID != "uid-abc" || c.Exp.Unix() != exp {
		t.Fatalf("unpadded claims: %+v", c)
	}
	// 带 padding
	padded := workbuddyTestJWT(t, "uid-pad", exp, true)
	if c := parseWorkbuddyJWTClaims(padded); c.UID != "uid-pad" || c.Exp.Unix() != exp {
		t.Fatalf("padded claims: %+v", c)
	}
	// 非法输入不 panic
	if c := parseWorkbuddyJWTClaims("not-a-jwt"); c.UID != "" || !c.Exp.IsZero() {
		t.Fatalf("garbage claims: %+v", c)
	}
}

func TestWorkBuddyParseEpochSecondsAndMillis(t *testing.T) {
	sec := float64(1789000000)
	if got := workbuddyParseEpoch(sec); !got.Equal(time.Unix(1789000000, 0)) {
		t.Fatalf("seconds: %v", got)
	}
	ms := float64(1789000000000)
	if got := workbuddyParseEpoch(ms); !got.Equal(time.UnixMilli(1789000000000)) {
		t.Fatalf("millis: %v", got)
	}
	if got := workbuddyParseEpoch("1789000000"); !got.Equal(time.Unix(1789000000, 0)) {
		t.Fatalf("string seconds: %v", got)
	}
	if got := workbuddyParseEpoch(nil); !got.IsZero() {
		t.Fatalf("nil: %v", got)
	}
}

func TestWorkBuddyNeedsRefreshWindow(t *testing.T) {
	wb := WorkBuddyIntl{}
	lead := 5 * time.Minute
	far := &oauth.TokenSet{ExpiresAt: time.Now().Add(2 * time.Hour)}
	if wb.NeedsRefresh(far, lead) {
		t.Fatal("far-future expiry must not need refresh")
	}
	soon := &oauth.TokenSet{ExpiresAt: time.Now().Add(2 * time.Minute)}
	if !wb.NeedsRefresh(soon, lead) {
		t.Fatal("expiry inside lead window must need refresh")
	}
}

func TestWorkBuddyExpiryFromExpiresAtMillis(t *testing.T) {
	now := time.Unix(1788000000, 0)
	ts := workbuddyTokenSet("at-plain", "rt-plain", "", "", time.UnixMilli(1789000000000), 0, now)
	if !ts.ExpiresAt.Equal(time.UnixMilli(1789000000000)) {
		t.Fatalf("expiresAt millis: %v", ts.ExpiresAt)
	}
	if ts.Extra["domain"] != "www.workbuddy.ai" || ts.Extra["realm"] != "intl" {
		t.Fatalf("extra defaults: %+v", ts.Extra)
	}
	// uid 缺省昵称：前 8 字符
	access := workbuddyTestJWT(t, "uid-1234567890", 0, false)
	ts2 := workbuddyTokenSet(access, "", "", "", time.Time{}, 0, now)
	if ts2.Extra["nickname"] != "uid-1234" {
		t.Fatalf("nickname fallback: %+v", ts2.Extra)
	}
}
