package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/workbuddy/upstream"
)

func TestCodeBuddyUnifiedUpstreamAndMetadata(t *testing.T) {
	access := workbuddyTestJWT(t, "uid-cn-test", time.Now().Add(time.Hour).Unix(), false)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("X-Domain") != "copilot.tencent.com" || r.Header.Get("User-Agent") != upstream.CLIUA {
			t.Errorf("改址不应改变账号身分: %v", r.Header)
		}
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			if r.Method != http.MethodPost || r.URL.Query().Get("platform") != "CLI" {
				t.Error("授权参数错误")
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"state-cn","authUrl":"https://www.codebuddy.cn/login"}}`))
		case "/v2/plugin/auth/token":
			if r.URL.Query().Get("state") != "state-cn" {
				t.Error("轮询 state 错误")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"accessToken": access, "refreshToken": "rt-cn", "expiresIn": 3600,
				"domain": "www.workbuddy.cn", "enterpriseId": "enterprise-test",
			}})
		case "/v2/plugin/auth/token/refresh":
			if r.Method != http.MethodPost || r.Header.Get("X-Refresh-Token") != "rt-cn" {
				t.Error("刷新参数错误")
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at-next","refreshToken":"rt-next","expiresIn":3600}}`))
		default:
			t.Errorf("未知端点: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Setenv("WB_CN_CHAT_UPSTREAM", server.URL)
	for _, name := range []string{"CODEBUDDY_STATE_URL", "CODEBUDDY_TOKEN_URL", "CODEBUDDY_REFRESH_URL"} {
		t.Setenv(name, "")
	}
	oldState, oldToken, oldRefresh := codebuddyStateURL, codebuddyTokenURL, codebuddyRefreshURL
	t.Cleanup(func() {
		codebuddyStateURL, codebuddyTokenURL, codebuddyRefreshURL = oldState, oldToken, oldRefresh
	})
	codebuddyStateURL = upstream.CNChatBaseDefault + "/v2/plugin/auth/state"
	codebuddyTokenURL = upstream.CNChatBaseDefault + "/v2/plugin/auth/token"
	codebuddyRefreshURL = upstream.CNChatBaseDefault + "/v2/plugin/auth/token/refresh"
	adapter := CodeBuddyCN{}
	callbacks := oauth.Callbacks{HTTPClient: server.Client()}
	_, temporary, err := adapter.BeginAuth(t.Context(), callbacks, "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := adapter.Resolve(t.Context(), callbacks, temporary)
	if err != nil {
		t.Fatal(err)
	}
	wantExtra := map[string]string{"uid": "uid-cn-test", "realm": "cn", "domain": "www.workbuddy.cn", "enterprise_id": "enterprise-test"}
	if !reflect.DeepEqual(token.Extra, wantExtra) {
		t.Fatalf("登录元信息未保留: %#v", token.Extra)
	}
	refreshed, err := adapter.Refresh(t.Context(), callbacks, token)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken != "rt-next" || !reflect.DeepEqual(refreshed.Extra, wantExtra) {
		t.Fatal("刷新应轮换 token 并保留账号元信息")
	}
	if want := []string{"/v2/plugin/auth/state", "/v2/plugin/auth/token", "/v2/plugin/auth/token/refresh"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("授权链路未完整改址: %v", paths)
	}
}

func TestBuddyRefreshRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"not-a-success","refreshToken":"not-a-success"}}`))
	}))
	defer server.Close()
	oldCN, oldIntl := codebuddyRefreshURL, workbuddyRefreshURL
	t.Cleanup(func() { codebuddyRefreshURL, workbuddyRefreshURL = oldCN, oldIntl })
	codebuddyRefreshURL, workbuddyRefreshURL = server.URL, server.URL
	for _, adapter := range []oauth.Adapter{CodeBuddyCN{}, WorkBuddyIntl{}} {
		t.Run(adapter.Key(), func(t *testing.T) {
			_, err := adapter.Refresh(t.Context(), oauth.Callbacks{HTTPClient: server.Client()}, &oauth.TokenSet{RefreshToken: "test-refresh"})
			if err == nil {
				t.Fatal("HTTP 401 不应按刷新成功处理")
			}
		})
	}
}
