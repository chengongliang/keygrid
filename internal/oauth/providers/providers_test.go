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

// ---- kimi device_code flow（mock 授权服务器）----

func TestKimiBeginAndPoll(t *testing.T) {
	// mock kimi auth server：device_authorization + token
	var pollCount int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			// 校验 client_id 与设备指纹头
			if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != kimiClientID {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"bad client"}`))
				return
			}
			if r.Header.Get("X-Msh-Device-Id") == "" || r.Header.Get("X-Msh-Platform") != "9router" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"missing device headers"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dc-123",
				"user_code":   "ABCD-EFGH",
				"expires_in":  300,
				"interval":    1,
			})
		case "/api/oauth/token":
			pollCount++
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				w.WriteHeader(400)
				return
			}
			if pollCount < 3 {
				// CLIProxyAPI: kimi pending = 200 + error field
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-kimi",
				"refresh_token": "rt-kimi",
				"expires_in":    3600,
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	// 劫持 endpoints 到 mock server
	origDevice, origToken := kimiDeviceCodeURL, kimiTokenURL
	defer func() { kimiDeviceCodeURL, kimiTokenURL = origDevice, origToken }()
	kimiDeviceCodeURL = up.URL + "/api/oauth/device_authorization"
	kimiTokenURL = up.URL + "/api/oauth/token"

	cb := oauth.Callbacks{Hostname: "test-host"}
	var kimi Kimi

	begin, temp, err := kimi.BeginAuth(t.Context(), cb, "state-1")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	if begin.UserCode != "ABCD-EFGH" || temp["device_code"] != "dc-123" {
		t.Fatalf("begin result: %+v temp=%v", begin, temp)
	}
	if temp["_kimiDeviceId"] == "" {
		t.Fatal("deviceId must be generated and kept in temp")
	}

	// 轮询：pending → pending → ok
	for i := 0; i < 2; i++ {
		_, err := kimi.Resolve(t.Context(), cb, temp)
		var pe *oauth.PendingError
		if !asPending(err, &pe) {
			t.Fatalf("poll %d: expected pending, got %v", i, err)
		}
	}
	tok, err := kimi.Resolve(t.Context(), cb, temp)
	if err != nil {
		t.Fatalf("final poll: %v", err)
	}
	if tok.AccessToken != "at-kimi" || tok.RefreshToken != "rt-kimi" {
		t.Fatalf("token: %+v", tok)
	}
	if tok.Extra["deviceId"] == "" {
		t.Fatal("kimi deviceId must persist into Extra (relay 请求头要用)")
	}
	if tok.ExpiresAt.IsZero() || tok.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expires_at wrong: %v", tok.ExpiresAt)
	}
}

func TestKimiRefresh(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("X-Msh-Device-Id") != "dev-9" {
			w.WriteHeader(400)
			return
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-old" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-new", "refresh_token": "", "expires_in": 1800,
		})
	}))
	defer up.Close()

	orig := kimiTokenURL
	defer func() { kimiTokenURL = orig }()
	kimiTokenURL = up.URL + "/api/oauth/token"

	kimi := Kimi{}
	tok := &oauth.TokenSet{
		AccessToken: "at-old", RefreshToken: "rt-old",
		Extra: map[string]string{"deviceId": "dev-9"},
	}
	newTok, err := kimi.Refresh(t.Context(), oauth.Callbacks{}, tok)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if newTok.AccessToken != "at-new" {
		t.Fatalf("new token: %+v", newTok)
	}
	// 9router: tokens.refresh_token || refreshToken → 空则保留旧值
	if newTok.RefreshToken != "rt-old" {
		t.Fatalf("refresh_token rotation fallback failed: %q", newTok.RefreshToken)
	}
	if newTok.Extra["deviceId"] != "dev-9" {
		t.Fatal("deviceId must survive refresh")
	}
}

// ---- 刷新错误分类（classifyOAuthRefreshError 1:1）----

func TestClassifyRefreshError(t *testing.T) {
	cases := []struct {
		err       *oauth.RefreshError
		permanent bool
		code      string
	}{
		{&oauth.RefreshError{Body: `{"error":"invalid_grant"}`}, true, "invalid_grant"},
		{&oauth.RefreshError{Body: `{"error":"refresh_token_reused"}`, OAuthError: "refresh_token_reused"}, true, "refresh_token_reused"},
		{&oauth.RefreshError{Body: "upstream blew up 500"}, false, ""},
		{&oauth.RefreshError{OAuthError: "invalid_request"}, true, "invalid_request"},
	}
	for i, c := range cases {
		perm, code := oauth.ClassifyRefreshError(c.err)
		if perm != c.permanent {
			t.Fatalf("case %d: permanent=%v want %v (err=%s)", i, perm, c.permanent, c.err.Error())
		}
		if c.code != "" && code != c.code {
			t.Fatalf("case %d: code=%q want %q", i, code, c.code)
		}
	}
	// 网络错误 → retryable
	if perm, _ := oauth.ClassifyRefreshError(mapErrNoKey{}); perm {
		t.Fatal("network error must be retryable")
	}
}

type mapErrNoKey struct{}

func (mapErrNoKey) Error() string { return "connection refused" }

// ---- pkce 工具 ----

func TestPKCEVerifierChallenge(t *testing.T) {
	v1, v2 := newCodeVerifier(), newCodeVerifier()
	if v1 == v2 || len(v1) < 43 {
		t.Fatalf("verifier weak: %q %q", v1, v2)
	}
	ch := s256Challenge(v1)
	if ch == "" || strings.ContainsAny(ch, "+/=") {
		t.Fatalf("challenge must be base64url: %q", ch)
	}
	// RFC 7636 附录 B 已知向量
	if got := s256Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("S256 vector mismatch: %q", got)
	}
}

// ---- openai / anthropic BeginAuth URL 形状 ----

func TestOpenAIBeginAuthURL(t *testing.T) {
	oa := OpenAI{}
	res, temp, err := oa.BeginAuth(t.Context(), oauth.Callbacks{PublicBaseURL: "https://gw.example.com"}, "st-1")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	for _, want := range []string{
		"response_type=code", "client_id=" + codexClientID,
		"code_challenge_method=S256", "codex_cli_simplified_flow=true",
		"state=st-1",
		// Codex 固定本机回调（OpenAI 只白名单 localhost:1455）
		"redirect_uri=" + url.QueryEscape(codexRedirectURI),
	} {
		if !strings.Contains(res.AuthorizeURL, want) {
			t.Fatalf("authorize url missing %q: %s", want, res.AuthorizeURL)
		}
	}
	if temp["code_verifier"] == "" {
		t.Fatal("verifier must be returned for storage")
	}
}

func TestAnthropicResolveCodeState(t *testing.T) {
	// claude: code 带 #state 后缀；JSON body exchange
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-claude", "refresh_token": "rt-claude",
			"expires_in": 3600, "scope": "user:inference",
		})
	}))
	defer up.Close()

	orig := claudeTokenURL
	defer func() { claudeTokenURL = orig }()
	claudeTokenURL = up.URL

	an := Anthropic{}
	tok, err := an.Resolve(t.Context(), oauth.Callbacks{}, map[string]string{
		"code": "authcode#st-42", "code_verifier": "v", "state": "st-42",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.AccessToken != "at-claude" || tok.Extra["scope"] != "user:inference" {
		t.Fatalf("token: %+v", tok)
	}
	if gotBody["code"] != "authcode" || gotBody["state"] != "st-42" || gotBody["grant_type"] != "authorization_code" {
		t.Fatalf("exchange body: %v", gotBody)
	}
}

// asPending errors.As 包装（避免直接 import errors 两次）。
func asPending(err error, target **oauth.PendingError) bool {
	if err == nil {
		return false
	}
	if pe, ok := err.(*oauth.PendingError); ok {
		*target = pe
		return true
	}
	return false
}
