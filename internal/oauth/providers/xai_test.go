package providers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

func TestXAIEndpointValidation(t *testing.T) {
	if _, err := validateXAIEndpoint("https://auth.x.ai/oauth2/token", "token_endpoint"); err != nil {
		t.Fatalf("valid endpoint rejected: %v", err)
	}
	for _, raw := range []string{
		"http://auth.x.ai/oauth2/token",
		"https://evil.example/oauth2/token",
		"https://x.ai.evil.example/oauth2/token",
	} {
		if _, err := validateXAIEndpoint(raw, "token_endpoint"); err == nil {
			t.Fatalf("endpoint %q should be rejected", raw)
		}
	}
}

func TestXAIAuthDeviceFlowAndRefresh(t *testing.T) {
	var pollCount int
	var refreshForm url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"device_authorization_endpoint": "https://auth.x.ai/oauth2/device_authorization",
				"token_endpoint":                "https://auth.x.ai/oauth2/token",
			})
		case "/oauth2/device_authorization":
			if r.Method != http.MethodPost {
				t.Fatalf("device method = %s, want POST", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatalf("device ParseForm: %v", err)
			}
			if r.Form.Get("client_id") != xaiClientID || r.Form.Get("scope") != xaiScope {
				t.Fatalf("device form = %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "device-xai",
				"user_code":                 "ABCD-1234",
				"verification_uri":          "https://grok.com/activate",
				"verification_uri_complete": "https://grok.com/activate?user_code=ABCD-1234",
				"expires_in":                600,
				"interval":                  2,
			})
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("token ParseForm: %v", err)
			}
			switch r.Form.Get("grant_type") {
			case xaiDeviceCodeGrant:
				pollCount++
				if pollCount == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  "access-xai",
					"refresh_token": "refresh-xai",
					"expires_in":    3600,
					"id_token":      testXAIJWT("user@x.ai", "subject-1"),
				})
			case "refresh_token":
				refreshForm = r.PostForm
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  "access-xai-new",
					"refresh_token": "refresh-xai-new",
					"expires_in":    7200,
				})
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	adapter := XAI{}
	cb := oauth.Callbacks{HTTPClient: rewriteHTTPClient(upstream.URL), Now: func() time.Time { return time.Unix(100, 0) }}
	begin, temp, err := adapter.BeginAuth(t.Context(), cb, "state")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	if begin.UserCode != "ABCD-1234" || begin.VerificationURIComp == "" {
		t.Fatalf("begin = %+v", begin)
	}
	if temp["device_code"] != "device-xai" || temp[xaiTokenEndpointKey] == "" {
		t.Fatalf("temp = %v", temp)
	}

	if _, err := adapter.Resolve(t.Context(), cb, temp); err == nil {
		t.Fatal("first resolve should be pending")
	} else {
		var pending *oauth.PendingError
		if !asPending(err, &pending) {
			t.Fatalf("first resolve error = %v, want pending", err)
		}
	}
	tok, err := adapter.Resolve(t.Context(), cb, temp)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if tok.AccessToken != "access-xai" || tok.RefreshToken != "refresh-xai" || tok.Extra["email"] != "user@x.ai" || tok.Extra["subject"] != "subject-1" {
		t.Fatalf("token = %+v", tok)
	}

	newTok, err := adapter.Refresh(t.Context(), cb, tok)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newTok.AccessToken != "access-xai-new" || newTok.RefreshToken != "refresh-xai-new" {
		t.Fatalf("refreshed token = %+v", newTok)
	}
	if refreshForm.Get("grant_type") != "refresh_token" || refreshForm.Get("client_id") != xaiClientID || refreshForm.Get("refresh_token") != "refresh-xai" {
		t.Fatalf("refresh form = %v", refreshForm)
	}
}

// rewriteHTTPClient 保留请求 URL 的 x.ai host，让 endpoint 校验仍按生产路径执行，
// 仅在测试 transport 层把请求转发到 httptest 服务。
func rewriteHTTPClient(serverURL string) *http.Client {
	base, _ := url.Parse(serverURL)
	return &http.Client{Transport: rewriteTransport{base: base}}
}

type rewriteTransport struct{ base *url.URL }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.base.Scheme
	clone.URL.Host = t.base.Host
	clone.Host = t.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func testXAIJWT(email, subject string) string {
	encode := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return encode(map[string]string{"alg": "none"}) + "." + encode(map[string]string{"email": email, "sub": subject}) + ".sig"
}

func TestXAIRefreshErrorIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer server.Close()
	_, err := (XAI{}).Refresh(t.Context(), oauth.Callbacks{HTTPClient: rewriteHTTPClient(server.URL)}, &oauth.TokenSet{
		RefreshToken: "old",
		Extra:        map[string]string{xaiTokenEndpointKey: "https://auth.x.ai/oauth2/token"},
	})
	if err == nil {
		t.Fatal("Refresh should fail")
	}
	var refreshErr *oauth.RefreshError
	if !errors.As(err, &refreshErr) || refreshErr.OAuthError != "invalid_grant" {
		t.Fatalf("error = %#v, want invalid_grant RefreshError", err)
	}
}
