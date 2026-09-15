package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
	"github.com/chengongliang/keygrid/internal/relay"
	"github.com/chengongliang/keygrid/internal/server/middleware"
	"github.com/chengongliang/keygrid/internal/server/resp"
	"github.com/chengongliang/keygrid/internal/ssrf"
)

// test.go POST /api/providers/{id}/test —— 渠道连通性测试。
// 发一个 max_tokens=1 的最小请求到上游 /v1/chat/completions，返回状态码/耗时/错误摘要。
// 不做模型校验（用 model_map 第一个或 "gpt-4o-mini" 占位），只验证「能连上+凭据被接受」。
type TestHandler struct {
	Op *op.Op
}

type testResult struct {
	OK          bool   `json:"ok"`
	Status      int    `json:"status"`
	LatencyMs   int64  `json:"latency_ms"`
	Error       string `json:"error,omitempty"`
	CredStatus  string `json:"credential_status"`
	ChannelName string `json:"channel_name"`
}

func (h *TestHandler) TestProvider(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.Op.GetProvider(userID, id)
	if err != nil {
		resp.NotFound(w, "provider not found")
		return
	}

	res := testResult{ChannelName: p.Name}

	// SSRF：私网 base_url 直接拒绝（admin 白名单场景后续按 role 放开）
	if err := ssrf.CheckBaseURL(p.BaseURL); err != nil {
		res.Error = err.Error()
		resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res})
		return
	}

	cred, err := h.Op.GetCredentialByProviderID(p.ID)
	if err != nil {
		res.Error = "credential not configured (oauth not authorized yet?)"
		resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res})
		return
	}
	res.CredStatus = cred.Status
	if cred.Status == "revoked" {
		res.Error = "credential revoked, re-auth required"
		resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res})
		return
	}

	secret, credExtra, err := credentialSecretForTest(cred)
	if err != nil {
		res.Error = err.Error()
		resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res})
		return
	}

	// 占位模型：model_map 第一个 key，否则 gpt-4o-mini
	modelName := "gpt-4o-mini"
	for k := range p.ModelMap {
		modelName = k
		break
	}

	client, err := probeClient(h.Op, p.UseProxy, 30*time.Second)
	if err != nil {
		res.Error = err.Error()
		resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res})
		return
	}
	status, latency, body := probeUpstream(r.Context(), client, p, secret, credExtra["chatgptAccountId"], modelName)
	res.Status = status
	res.LatencyMs = latency
	// 上游 401/403 → 凭据问题；404 → base_url 路径不对；200/400（参数错）→ 连通即可
	switch {
	case status == 0:
		res.Error = body
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		res.Error = "upstream rejected credential (" + body + ")"
	default:
		res.OK = true
		if status >= 400 {
			// 404/参数错误等：网络通，但配置可能有问题，附带响应摘要
			res.Error = "reachable but upstream returned " + itoa(status) + ": " + body
		}
	}
	resp.JSON(w, http.StatusOK, map[string]any{"code": 0, "data": res})
}

// credentialSecretForTest 解密凭据取上游密钥（api_key 或 oauth access_token）及
// token extra（codex 探测需要 extra.chatgptAccountId 拼 chatgpt-account-id 头）。
// 注意：不触发 oauth 兜底刷新（test 是只读轻操作），过期 token 由上游 401 反映。
func credentialSecretForTest(cred *model.Credential) (string, map[string]string, error) {
	plain, err := crypto.Decrypt(cred.EncData)
	if err != nil {
		return "", nil, err
	}
	var d oauth.TokenSetData
	if err := json.Unmarshal(plain, &d); err != nil || d.AccessToken == "" {
		var kd struct {
			APIKey string `json:"api_key"`
		}
		if err2 := json.Unmarshal(plain, &kd); err2 != nil || kd.APIKey == "" {
			return "", nil, errors.New("invalid credential data")
		}
		return kd.APIKey, nil, nil
	}
	return d.AccessToken, d.Extra, nil
}

// probeUpstream 发最小探活请求：普通渠道打 /v1/chat/completions；
// Codex 渠道打 base_url（Responses endpoint，store:false 最小 input）。
// client 由调用方按渠道代理开关构建（probeClient）。
func probeUpstream(ctx context.Context, client *http.Client, p *model.Provider, secret, accountID, model string) (int, int64, string) {
	var target string
	var payload []byte
	if relay.IsCodexProvider(p) {
		target = strings.TrimRight(p.BaseURL, "/")
		payload, _ = json.Marshal(map[string]any{
			"model":  model,
			"input":  []any{},
			"stream": true,
			"store":  false,
		})
	} else {
		target = strings.TrimRight(p.BaseURL, "/") + "/v1/chat/completions"
		payload, _ = json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	if relay.IsCodexProvider(p) {
		// 9router codex transport.headers + chatgpt-account-id
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
		if accountID != "" {
			req.Header.Set("chatgpt-account-id", accountID)
		}
	}

	start := time.Now()
	respUp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return 0, latency, err.Error()
	}
	defer respUp.Body.Close()
	buf := make([]byte, 256)
	n, _ := respUp.Body.Read(buf)
	return respUp.StatusCode, latency, string(buf[:n])
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
