package relay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chengongliang/keygrid/internal/model"
)

func TestQoderCOSYUsesPrintableCLICompatibleAESKey(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	previousPublicKey := qoderPublicKey
	qoderPublicKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	defer func() { qoderPublicKey = previousPublicKey }()

	headers, err := qoderCosyHeaders(nil, "https://gateway.qoder.com.cn/algo/api/v2/model/list", qoderAuth{
		token: "dt-test", userID: "qoder-user-7", name: "Qoder User", email: "qoder@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	wrappedKey, err := base64.StdEncoding.DecodeString(headers.Get("Cosy-Key"))
	if err != nil {
		t.Fatal(err)
	}
	aesKey, err := rsa.DecryptPKCS1v15(rand.Reader, privateKey, wrappedKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(aesKey) != aes.BlockSize {
		t.Fatalf("AES key length = %d, want %d", len(aesKey), aes.BlockSize)
	}
	for _, b := range aesKey {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') && b != '-' {
			t.Fatalf("Qoder AES key contains non-CLI-compatible byte %q", b)
		}
	}

	authorization := strings.TrimPrefix(headers.Get("Authorization"), "Bearer COSY.")
	parts := strings.Split(authorization, ".")
	if len(parts) != 2 {
		t.Fatalf("malformed COSY authorization: %q", headers.Get("Authorization"))
	}
	var payload struct {
		Info string `json:"info"`
	}
	payloadJSON, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(payloadJSON, &payload) != nil {
		t.Fatalf("decode COSY payload: %v", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(payload.Info)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		t.Fatalf("encrypted user info length = %d, not block aligned", len(ciphertext))
	}
	cipher.NewCBCDecrypter(block, aesKey).CryptBlocks(ciphertext, ciphertext)
	padding := int(ciphertext[len(ciphertext)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(ciphertext) {
		t.Fatalf("invalid user info padding %d", padding)
	}
	var userInfo map[string]string
	if err := json.Unmarshal(ciphertext[:len(ciphertext)-padding], &userInfo); err != nil {
		t.Fatalf("decrypt user info: %v", err)
	}
	if userInfo["uid"] != "qoder-user-7" || userInfo["security_oauth_token"] != "dt-test" {
		t.Fatalf("decrypted user info = %#v", userInfo)
	}
}

func TestQoderPATExchangeCOSYAndNonStreamingCompletion(t *testing.T) {
	oldOpenAPI, oldAPI3, oldAPI2 := qoderIntlOpenAPI, qoderIntlAPI3, qoderIntlAPI2
	defer func() { qoderIntlOpenAPI, qoderIntlAPI3, qoderIntlAPI2 = oldOpenAPI, oldAPI3, oldAPI2 }()
	qoderCacheMu.Lock()
	qoderPATs = map[string]qoderPATCredential{}
	qoderCatalog = map[string]qoderModelCatalog{}
	qoderCacheMu.Unlock()
	defer func() {
		qoderCacheMu.Lock()
		qoderPATs = map[string]qoderPATCredential{}
		qoderCatalog = map[string]qoderModelCatalog{}
		qoderCacheMu.Unlock()
	}()

	var sawChat bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/jobToken/exchange":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["personal_token"] != "pt-test" || r.Header.Get("Cosy-Version") != qoderCOSYVersion {
				http.Error(w, "bad exchange request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "jt-test", "expires_in": 3600})
		case r.URL.Path == "/api/v1/userinfo":
			if r.Header.Get("Authorization") != "Bearer jt-test" {
				http.Error(w, "bad userinfo token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "qoder-user-7"})
		case r.URL.Path == "/algo/api/v2/model/list":
			if r.Header.Get("Cosy-User") != "qoder-user-7" || r.Header.Get("Cosy-Sigpath") != "/api/v2/model/list" {
				http.Error(w, "bad model list signature", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"chat": []any{map[string]any{"key": "qmodel", "display_name": "Q Model", "source": "system", "max_output_tokens": 8192}}})
		case r.URL.Path == "/algo/api/v2/service/pro/sse/agent_chat_generation":
			sawChat = true
			if r.Method != http.MethodPost || r.URL.Query().Get("Encode") != "1" || r.Header.Get("Cosy-User") != "qoder-user-7" || r.Header.Get("Cosy-Sigpath") != "/api/v2/service/pro/sse/agent_chat_generation" {
				http.Error(w, "bad chat request", http.StatusUnauthorized)
				return
			}
			encoded, _ := io.ReadAll(r.Body)
			if got := r.Header.Get("Cosy-Bodyhash"); got != md5Hex(encoded) || r.Header.Get("Cosy-Bodylength") != fmt.Sprint(len(encoded)) {
				http.Error(w, "bad body signature headers", http.StatusUnauthorized)
				return
			}
			if !validQoderSignature(r.Header.Get("Authorization"), r.Header.Get("Cosy-Key"), r.Header.Get("Cosy-Date"), r.Header.Get("Cosy-Sigpath"), encoded) {
				http.Error(w, "bad COSY signature", http.StatusUnauthorized)
				return
			}
			plain, err := decodeQoderBody(encoded)
			if err != nil {
				http.Error(w, "body encoding failed", http.StatusBadRequest)
				return
			}
			var payload map[string]any
			if json.Unmarshal(plain, &payload) != nil || payload["model_config"] == nil || payload["stream"] != true {
				http.Error(w, "invalid Qoder payload", http.StatusBadRequest)
				return
			}
			config, _ := payload["model_config"].(map[string]any)
			if config["key"] != "qmodel" {
				http.Error(w, "wrong model config", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"statusCodeValue\":200,\"body\":\"{\\\"id\\\":\\\"cmpl-1\\\",\\\"choices\\\":[{\\\"index\\\":0,\\\"delta\\\":{\\\"content\\\":\\\"hello\\\",\\\"finish_reason\\\":\\\"stop\\\"}}]}\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[],\\\"usage\\\":{\\\"prompt_tokens\\\":11,\\\"completion_tokens\\\":3}}\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	qoderIntlOpenAPI, qoderIntlAPI3, qoderIntlAPI2 = up.URL, up.URL, up.URL

	h := &Handler{}
	provider := &model.Provider{Kind: "api_key", OAuthProvider: "qoder"}
	body := []byte(`{"model":"qmodel","messages":[{"role":"system","content":"answer briefly"},{"role":"user","content":"hello"}],"stream":false}`)
	w := httptest.NewRecorder()
	status, usage, err := h.upstreamCallQoder(t.Context(), up.Client(), "pt-test", nil, provider, "qmodel", body, false, protoOpenAI, w, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("qoder call status=%d err=%v body=%s", status, err, w.Body.String())
	}
	if !sawChat || usage.PromptTokens != 11 || usage.CompletionTokens != 3 {
		t.Fatalf("chat=%v usage=%+v", sawChat, usage)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &completion); err != nil || len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "hello" || completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("completion=%s err=%v", w.Body.String(), err)
	}
	probe := QoderProbe(t.Context(), up.Client(), "pt-test", nil, provider, "qmodel", "hello", 3, true)
	if probe.Error != "" || probe.Status != http.StatusOK || probe.Content != "hello" || probe.FinishReason != "stop" || probe.PromptTokens != 11 {
		t.Fatalf("stream probe = %+v", probe)
	}
	models, err := QoderModels(t.Context(), up.Client(), "pt-test", nil, provider)
	if err != nil || !containsString(models, "qmodel") || !containsString(models, "smodel") {
		t.Fatalf("model list = %v err=%v", models, err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestQoderStreamUnwrapMovesFinishReason(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\",\\\"finish_reason\\\":\\\"stop\\\"}}]}\"}\n\ndata: [DONE]\n\n"))
	inner, status, err := qoderUnwrapSSE(body)
	if err != nil || status != http.StatusOK {
		t.Fatalf("unwrap status=%d err=%v", status, err)
	}
	defer inner.Close()
	data, err := io.ReadAll(inner)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"finish_reason":"stop"`) || strings.Contains(string(data), `"delta":{"content":"ok","finish_reason"`) {
		t.Fatalf("finish reason was not normalized: %s", data)
	}
}

func TestQoderRegionEndpointSelection(t *testing.T) {
	if got := qoderChatBase("intl", "dt-device"); got != qoderIntlAPI3 {
		t.Fatalf("device token base = %q", got)
	}
	if got := qoderChatBase("intl", "jt-job"); got != qoderIntlAPI2 {
		t.Fatalf("job token base = %q", got)
	}
	if got := qoderChatBase("cn", "jt-job"); got != qoderCNGateway {
		t.Fatalf("CN token base = %q", got)
	}
}

func TestQoderCNModelsUsesGatewayAndCOSY(t *testing.T) {
	previousOpenAPI, previousGateway := qoderCNOpenAPI, qoderCNGateway
	qoderCacheMu.Lock()
	previousCatalog := qoderCatalog
	qoderCatalog = map[string]qoderModelCatalog{}
	qoderCacheMu.Unlock()
	defer func() {
		qoderCNOpenAPI, qoderCNGateway = previousOpenAPI, previousGateway
		qoderCacheMu.Lock()
		qoderCatalog = previousCatalog
		qoderCacheMu.Unlock()
	}()

	var sawModelList bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/userinfo":
			if r.Header.Get("Authorization") != "Bearer dt-cn" {
				http.Error(w, "bad userinfo token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"userId": "qoder-cn-user"})
		case "/algo/api/v2/model/list":
			sawModelList = true
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "failed to read body", http.StatusBadRequest)
				return
			}
			if r.Method != http.MethodGet || r.Header.Get("Cosy-User") != "qoder-cn-user" || r.Header.Get("Cosy-Sigpath") != "/api/v2/model/list" || !validQoderSignature(r.Header.Get("Authorization"), r.Header.Get("Cosy-Key"), r.Header.Get("Cosy-Date"), r.Header.Get("Cosy-Sigpath"), body) {
				http.Error(w, "bad CN COSY signature", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"chat": []any{map[string]any{"key": "qmodel-cn"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	qoderCNOpenAPI, qoderCNGateway = up.URL, up.URL

	models, err := QoderModels(t.Context(), up.Client(), "dt-cn", nil, &model.Provider{Kind: "oauth", OAuthProvider: "qoder-cn"})
	if err != nil || !sawModelList || !containsString(models, "qmodel-cn") {
		t.Fatalf("CN models = %v, request=%v, err=%v", models, sawModelList, err)
	}
}

func TestQoderProbeUsesStoredOAuthIdentity(t *testing.T) {
	previousOpenAPI, previousAPI3 := qoderIntlOpenAPI, qoderIntlAPI3
	qoderCacheMu.Lock()
	previousCatalog := qoderCatalog
	qoderCatalog = map[string]qoderModelCatalog{}
	qoderCacheMu.Unlock()
	defer func() {
		qoderIntlOpenAPI, qoderIntlAPI3 = previousOpenAPI, previousAPI3
		qoderCacheMu.Lock()
		qoderCatalog = previousCatalog
		qoderCacheMu.Unlock()
	}()

	var sawChat bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v2/model/list":
			if r.Header.Get("Cosy-User") != "oauth-user" || r.Header.Get("Cosy-Machineid") != "oauth-machine-id" {
				http.Error(w, "stored OAuth identity was not used", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"chat": []any{map[string]any{"key": "dfmodel", "source": "system"}}})
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			sawChat = true
			if r.Header.Get("Cosy-User") != "oauth-user" || r.Header.Get("Cosy-Machineid") != "oauth-machine-id" || r.Header.Get("X-Model-Key") != "dfmodel" {
				http.Error(w, "stored OAuth identity was not used", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"statusCodeValue\":200,\"body\":\"{\\\"id\\\":\\\"cmpl-1\\\",\\\"choices\\\":[{\\\"index\\\":0,\\\"delta\\\":{\\\"content\\\":\\\"ok\\\",\\\"finish_reason\\\":\\\"stop\\\"}}]}\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	qoderIntlOpenAPI, qoderIntlAPI3 = up.URL, up.URL

	probe := QoderProbe(t.Context(), up.Client(), "dt-oauth", map[string]string{
		"userId": "oauth-user", "displayName": "OAuth User", "email": "oauth@example.com", "machineId": "oauth-machine-id",
	}, &model.Provider{Kind: "oauth", OAuthProvider: "qoder"}, "dfmodel", "ping", 1, true)
	if !sawChat || probe.Status != http.StatusOK || probe.Error != "" || probe.Content != "ok" {
		t.Fatalf("probe = %+v, saw chat=%v", probe, sawChat)
	}
}

func TestQoderBuildPayloadPreservesToolCallContext(t *testing.T) {
	input := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-1","name":"lookup","content":"result"}]}`)
	payloadBytes, err := qoderBuildPayload("qmodel", map[string]any{"key": "qmodel", "source": "system"}, input, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) != 2 || payload.Messages[0]["tool_calls"] == nil || payload.Messages[1]["tool_call_id"] != "call-1" || payload.Messages[1]["name"] != "lookup" {
		t.Fatalf("tool call context lost: %s", payloadBytes)
	}
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func validQoderSignature(authorization, cosyKey, timestamp, sigPath string, body []byte) bool {
	value := strings.TrimPrefix(authorization, "Bearer COSY.")
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	input := []byte(parts[0] + "\n" + cosyKey + "\n" + timestamp + "\n")
	input = append(input, body...)
	input = append(input, []byte("\n"+sigPath)...)
	return md5Hex(input) == parts[1]
}

func decodeQoderBody(body []byte) ([]byte, error) {
	standard := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/")
	custom := []byte(qoderEncodingAlphabet)
	decoded := append([]byte(nil), body...)
	for i, char := range decoded {
		if char == '$' {
			decoded[i] = '='
			continue
		}
		idx := bytes.IndexByte(custom, char)
		if idx < 0 {
			return nil, fmt.Errorf("unknown encoded byte %q", char)
		}
		decoded[i] = standard[idx]
	}
	n := len(decoded)
	a := n / 3
	rearranged := make([]byte, 0, n)
	rearranged = append(rearranged, decoded[n-a:]...)
	rearranged = append(rearranged, decoded[a:n-a]...)
	rearranged = append(rearranged, decoded[:a]...)
	return base64.StdEncoding.DecodeString(string(rearranged))
}
