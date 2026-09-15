package providers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// ---- OpenAI Codex：PKCE + ChatGPT account claims ----

// codexTestJWT 构造假 id_token（header.payload.signature，payload 带指定 claims）。
func codexTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b64 := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return b64(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + b64(claims) + ".c2ln"
}

func TestOpenAIBeginAuthRedirectURI(t *testing.T) {
	begin, temp, err := (OpenAI{}).BeginAuth(t.Context(), oauth.Callbacks{PublicBaseURL: "https://gw.example.com"}, "st-x")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	// 9router codex：固定本机回调（平台域名 redirect 不被 OpenAI 接受）
	if !strings.Contains(begin.AuthorizeURL, "redirect_uri="+url.QueryEscape(codexRedirectURI)) {
		t.Fatalf("authorize url must use fixed localhost redirect: %s", begin.AuthorizeURL)
	}
	if begin.RedirectURI != codexRedirectURI {
		t.Fatalf("BeginResult.RedirectURI = %q", begin.RedirectURI)
	}
	for _, want := range []string{
		"https://auth.openai.com/oauth/authorize?",
		"client_id=" + codexClientID,
		"code_challenge_method=S256",
		"state=st-x",
		"originator=codex_cli_rs",
		"codex_cli_simplified_flow=true",
		"id_token_add_organizations=true",
	} {
		if !strings.Contains(begin.AuthorizeURL, want) {
			t.Fatalf("authorize url missing %q: %s", want, begin.AuthorizeURL)
		}
	}
	if temp["code_verifier"] == "" || temp["redirect_uri"] != codexRedirectURI {
		t.Fatalf("temp must carry verifier + redirect_uri: %v", temp)
	}
	// challenge 与 verifier 匹配
	u, _ := url.Parse(begin.AuthorizeURL)
	q := u.Query()
	if q.Get("code_challenge") != s256Challenge(temp["code_verifier"]) {
		t.Fatal("code_challenge must be s256(code_verifier)")
	}
}

func TestOpenAIResolveAndRefresh(t *testing.T) {
	idToken := codexTestJWT(t, map[string]any{
		"email": "dev@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-123",
			"chatgpt_plan_type":  "plus",
		},
	})
	var resolveForm, refreshJSON map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Content-Type") {
		case "application/x-www-form-urlencoded":
			_ = r.ParseForm()
			resolveForm = map[string]any{}
			for k := range r.Form {
				resolveForm[k] = r.Form.Get(k)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-cx",
				"refresh_token": "rt-cx",
				"id_token":      idToken,
				"expires_in":    3600,
			})
		case "application/json":
			_ = json.NewDecoder(r.Body).Decode(&refreshJSON)
			// refresh 后轮换 account id
			newID := codexTestJWT(t, map[string]any{
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_account_id": "acct-456",
					"chatgpt_plan_type":  "pro",
				},
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-cx2",
				"refresh_token": "rt-cx2",
				"id_token":      newID,
				"expires_in":    7200,
			})
		default:
			w.WriteHeader(415)
		}
	}))
	defer up.Close()
	orig := codexTokenURL
	defer func() { codexTokenURL = orig }()
	codexTokenURL = up.URL

	cb := oauth.Callbacks{PublicBaseURL: "https://gw.example.com"}
	var oa OpenAI

	// Resolve：form body + 固定 redirect_uri + account claims 进 extra
	temp := map[string]string{"code": "ac-1", "code_verifier": "v-" + strings.Repeat("x", 40), "redirect_uri": codexRedirectURI}
	tok, err := oa.Resolve(t.Context(), cb, temp)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.AccessToken != "at-cx" || tok.RefreshToken != "rt-cx" {
		t.Fatalf("token set: %+v", tok)
	}
	if resolveForm["grant_type"] != "authorization_code" || resolveForm["client_id"] != codexClientID ||
		resolveForm["code"] != "ac-1" || resolveForm["redirect_uri"] != codexRedirectURI {
		t.Fatalf("resolve form: %v", resolveForm)
	}
	if tok.Extra["chatgptAccountId"] != "acct-123" || tok.Extra["chatgptPlanType"] != "plus" || tok.Extra["email"] != "dev@example.com" {
		t.Fatalf("extra claims: %v", tok.Extra)
	}

	// Refresh：JSON body（9router refreshCodexToken）+ account id 更新
	newTok, err := oa.Refresh(t.Context(), cb, tok)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newTok.AccessToken != "at-cx2" || newTok.RefreshToken != "rt-cx2" {
		t.Fatalf("refresh token set: %+v", newTok)
	}
	if refreshJSON["grant_type"] != "refresh_token" || refreshJSON["client_id"] != codexClientID || refreshJSON["refresh_token"] != "rt-cx" {
		t.Fatalf("refresh json: %v", refreshJSON)
	}
	if newTok.Extra["chatgptAccountId"] != "acct-456" || newTok.Extra["chatgptPlanType"] != "pro" {
		t.Fatalf("refresh extra claims: %v", newTok.Extra)
	}
}

func TestParseChatGPTClaimsFallbacks(t *testing.T) {
	// 9router YR：顶层 account_id 回退
	tok := codexTestJWT(t, map[string]any{"account_id": "acct-top"})
	if c := parseChatGPTClaims(tok); c.AccountID != "acct-top" {
		t.Fatalf("top-level account_id fallback: %+v", c)
	}
	// 畸形 token 安全返回空
	if c := parseChatGPTClaims("not-a-jwt"); c.AccountID != "" {
		t.Fatalf("malformed token should yield empty: %+v", c)
	}
	if c := parseChatGPTClaims(""); c.AccountID != "" {
		t.Fatalf("empty token should yield empty: %+v", c)
	}
}
