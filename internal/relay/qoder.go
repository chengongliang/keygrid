package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
)

// Qoder 的 PAT 和 device token 都必须经过 COSY 请求签名，不能走标准 Bearer 转发。
var (
	qoderIntlOpenAPI = "https://openapi.qoder.sh"
	qoderCNOpenAPI   = "https://openapi.qoder.com.cn"
	qoderIntlAPI3    = "https://api3.qoder.sh"
	qoderIntlAPI2    = "https://api2.qoder.sh"
	qoderCNGateway   = "https://gateway.qoder.com.cn"
	qoderPublicKey   = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`
	qoderCacheMu sync.Mutex
	qoderPATs    = map[string]qoderPATCredential{}
	qoderCatalog = map[string]qoderModelCatalog{}
)

const qoderCOSYVersion = "1.0.0"

var qoderEncodingAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

type qoderPATCredential struct {
	token     string
	userID    string
	validTill time.Time
}

type qoderModelCatalog struct {
	configs   map[string]map[string]any
	validTill time.Time
}

type qoderAuth struct {
	token     string
	userID    string
	name      string
	email     string
	machineID string
}

func IsQoderProvider(p *model.Provider) bool {
	return p != nil && (p.OAuthProvider == "qoder" || p.OAuthProvider == "qoder-cn")
}

func qoderRegion(p *model.Provider) string {
	if p != nil && p.OAuthProvider == "qoder-cn" {
		return "cn"
	}
	return "intl"
}

func qoderOpenAPI(region string) string {
	if region == "cn" {
		return qoderCNOpenAPI
	}
	return qoderIntlOpenAPI
}

func qoderChatBase(region, token string) string {
	if region == "cn" {
		return qoderCNGateway
	}
	if strings.HasPrefix(token, "jt-") {
		return qoderIntlAPI2
	}
	return qoderIntlAPI3
}

func qoderUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func qoderStableHash(parts ...string) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = io.WriteString(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func qoderPATCacheKey(region, pat string) string {
	h := sha256.Sum256([]byte(pat))
	return region + ":" + hex.EncodeToString(h[:])
}

func (h *Handler) resolveQoderAuth(ctx context.Context, client *http.Client, token string, extra map[string]string, region string) (qoderAuth, error) {
	auth := qoderAuth{token: token}
	if strings.HasPrefix(token, "pt-") {
		key := qoderPATCacheKey(region, token)
		qoderCacheMu.Lock()
		cached, ok := qoderPATs[key]
		qoderCacheMu.Unlock()
		if !ok || time.Until(cached.validTill) <= 5*time.Minute {
			resolved, err := h.exchangeQoderPAT(ctx, client, token, region)
			if err != nil {
				return qoderAuth{}, err
			}
			cached = resolved
			qoderCacheMu.Lock()
			qoderPATs[key] = cached
			qoderCacheMu.Unlock()
		}
		auth.token, auth.userID = cached.token, cached.userID
	} else {
		auth.userID = extra["userId"]
		auth.name = extra["displayName"]
		auth.email = extra["email"]
		auth.machineID = extra["machineId"]
	}
	if auth.userID == "" {
		id, err := h.qoderUserID(ctx, client, auth.token, region)
		if err != nil {
			return qoderAuth{}, err
		}
		auth.userID = id
	}
	if auth.userID == "" || auth.token == "" {
		return qoderAuth{}, &UpstreamError{Status: http.StatusUnauthorized, Body: `{"error":{"message":"qoder credential is missing user id or access token"}}`, ContentType: "application/json"}
	}
	if auth.machineID == "" {
		hash := sha256.Sum256([]byte("qoder-machine\x00" + auth.userID))
		hexID := hex.EncodeToString(hash[:16])
		auth.machineID = hexID[:8] + "-" + hexID[8:12] + "-5" + hexID[13:16] + "-a" + hexID[17:20] + "-" + hexID[20:32]
	}
	return auth, nil
}

func (h *Handler) exchangeQoderPAT(ctx context.Context, client *http.Client, pat, region string) (qoderPATCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	u := qoderOpenAPI(region) + "/api/v1/jobToken/exchange"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return qoderPATCredential{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qodercli/1.0.0")
	req.Header.Set("Cosy-Version", qoderCOSYVersion)
	req.Header.Set("Cosy-ClientType", "5")
	resp, err := client.Do(req)
	if err != nil {
		return qoderPATCredential{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return qoderPATCredential{}, &UpstreamError{Status: resp.StatusCode, Body: string(b), ContentType: resp.Header.Get("Content-Type")}
	}
	var data struct {
		Token     string          `json:"token"`
		ExpiresAt json.RawMessage `json:"expires_at"`
		ExpiresIn json.RawMessage `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&data); err != nil || data.Token == "" {
		return qoderPATCredential{}, errors.New("qoder PAT exchange returned no job token")
	}
	validTill := time.Now().Add(24 * time.Hour)
	if len(data.ExpiresAt) > 0 && string(data.ExpiresAt) != "null" {
		var text string
		if json.Unmarshal(data.ExpiresAt, &text) == nil {
			if t, err := time.Parse(time.RFC3339, text); err == nil {
				validTill = t
			}
		} else {
			var epoch int64
			if json.Unmarshal(data.ExpiresAt, &epoch) == nil && epoch > 0 {
				if epoch < 1_000_000_000_000 {
					validTill = time.Unix(epoch, 0)
				} else {
					validTill = time.UnixMilli(epoch)
				}
			}
		}
	} else {
		var expiresIn int64
		if json.Unmarshal(data.ExpiresIn, &expiresIn) == nil && expiresIn > 0 {
			// Qoder variants report either OAuth seconds or JavaScript milliseconds.
			if expiresIn > 1_000_000 {
				validTill = time.Now().Add(time.Duration(expiresIn) * time.Millisecond)
			} else {
				validTill = time.Now().Add(time.Duration(expiresIn) * time.Second)
			}
		}
	}
	userID, err := h.qoderUserID(ctx, client, data.Token, region)
	if err != nil {
		return qoderPATCredential{}, err
	}
	return qoderPATCredential{token: data.Token, userID: userID, validTill: validTill}, nil
}

func (h *Handler) qoderUserID(ctx context.Context, client *http.Client, token, region string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoderOpenAPI(region)+"/api/v1/userinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qodercli/1.0.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("qoder userinfo failed (%d)", resp.StatusCode)
	}
	var data struct {
		ID     string `json:"id"`
		UserID string `json:"userId"`
		Snake  string `json:"user_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&data); err != nil {
		return "", errors.New("qoder userinfo response is invalid")
	}
	for _, id := range []string{data.ID, data.UserID, data.Snake} {
		if strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id), nil
		}
	}
	return "", errors.New("qoder userinfo did not return user id")
}

func qoderModelListURL(region, token string) string {
	return qoderChatBase(region, token) + "/algo/api/v2/model/list"
}

func qoderChatURL(region, token string) string {
	return qoderChatBase(region, token) + "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
}

// QoderModels 通过账号级 COSY 模型目录供渠道向导和已存渠道模型探测使用。
func QoderModels(ctx context.Context, client *http.Client, token string, extra map[string]string, provider *model.Provider) ([]string, error) {
	if !IsQoderProvider(provider) {
		return nil, errors.New("provider is not Qoder")
	}
	h := &Handler{}
	region := qoderRegion(provider)
	auth, err := h.resolveQoderAuth(ctx, client, token, extra, region)
	if err != nil {
		return nil, err
	}
	catalog, err := h.loadQoderCatalog(ctx, client, auth, region)
	if err != nil {
		var upstream *UpstreamError
		if errors.As(err, &upstream) {
			if detail := sanitizeErrMessage(upstream.Body, upstream.ContentType); detail != "" {
				return nil, fmt.Errorf("%w: %s", err, detail)
			}
		}
		return nil, err
	}
	seen := make(map[string]bool, len(catalog.configs))
	models := make([]string, 0, len(catalog.configs))
	for key := range catalog.configs {
		seen[key] = true
		models = append(models, key)
	}
	if preset, ok := oauth.LookupPreset(provider.OAuthProvider); ok {
		for _, key := range preset.Models {
			if !seen[key] {
				seen[key] = true
				models = append(models, key)
			}
		}
	}
	sort.Strings(models)
	return models, nil
}

func (h *Handler) loadQoderCatalog(ctx context.Context, client *http.Client, auth qoderAuth, region string) (qoderModelCatalog, error) {
	cacheKey := qoderStableHash(region, auth.userID)
	qoderCacheMu.Lock()
	cached, ok := qoderCatalog[cacheKey]
	qoderCacheMu.Unlock()
	if !ok || time.Until(cached.validTill) <= 0 {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		u := qoderModelListURL(region, auth.token)
		headers, err := qoderCosyHeaders(nil, u, auth)
		if err != nil {
			return qoderModelCatalog{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return qoderModelCatalog{}, err
		}
		req.Header = headers
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		if err != nil {
			return qoderModelCatalog{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			return qoderModelCatalog{}, &UpstreamError{Status: resp.StatusCode, Body: string(b), ContentType: resp.Header.Get("Content-Type")}
		}
		var data struct {
			Chat []map[string]any `json:"chat"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&data); err != nil {
			return qoderModelCatalog{}, errors.New("qoder model list response is invalid")
		}
		configs := make(map[string]map[string]any, len(data.Chat))
		for _, entry := range data.Chat {
			key, _ := entry["key"].(string)
			if key != "" {
				configs[key] = entry
			}
		}
		cached = qoderModelCatalog{configs: configs, validTill: time.Now().Add(time.Hour)}
		qoderCacheMu.Lock()
		qoderCatalog[cacheKey] = cached
		qoderCacheMu.Unlock()
	}
	return cached, nil
}

func (h *Handler) qoderModelConfig(ctx context.Context, client *http.Client, auth qoderAuth, region, modelKey string) (map[string]any, error) {
	cached, err := h.loadQoderCatalog(ctx, client, auth, region)
	if err != nil {
		return nil, err
	}
	if config := cached.configs[modelKey]; config != nil {
		return config, nil
	}
	if modelKey == "smodel" || modelKey == "cmodel" {
		name := "Sonus"
		if modelKey == "cmodel" {
			name = "Cantus"
		}
		return map[string]any{"key": modelKey, "display_name": name, "format": "openai", "source": "system", "is_vl": true, "is_reasoning": true, "max_input_tokens": 180000}, nil
	}
	return nil, &UpstreamError{Status: http.StatusBadRequest, Body: fmt.Sprintf(`{"error":{"message":"qoder model %q is not present in the account model list"}}`, modelKey), ContentType: "application/json"}
}

func qoderEncodeBody(plain []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(plain)
	n := len(encoded)
	a := n / 3
	rearranged := encoded[n-a:] + encoded[a:n-a] + encoded[:a]
	out := []byte(rearranged)
	for i, c := range out {
		if c == '=' {
			out[i] = '$'
			continue
		}
		idx := strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", c)
		if idx >= 0 {
			out[i] = qoderEncodingAlphabet[idx]
		}
	}
	return out
}

func qoderCosyHeaders(body []byte, target string, auth qoderAuth) (http.Header, error) {
	block, _ := pem.Decode([]byte(qoderPublicKey))
	if block == nil {
		return nil, errors.New("qoder COSY public key is invalid")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("qoder COSY public key is not RSA")
	}
	// QoderCLI wraps the first 16 ASCII bytes of a UUID as the AES key. Keep
	// the wire format printable: its backend converts the RSA plaintext to text.
	aesKey := []byte(qoderUUID()[:16])
	userInfo, _ := json.Marshal(map[string]string{"uid": auth.userID, "security_oauth_token": auth.token, "name": auth.name, "aid": "", "email": auth.email})
	padded := pkcs7Pad(userInfo, aes.BlockSize)
	blockCipher, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	encInfo := make([]byte, len(padded))
	cipher.NewCBCEncrypter(blockCipher, aesKey).CryptBlocks(encInfo, padded)
	infoB64 := base64.StdEncoding.EncodeToString(encInfo)
	cosyKey, err := rsa.EncryptPKCS1v15(rand.Reader, publicKey, aesKey)
	if err != nil {
		return nil, err
	}
	cosyKeyB64 := base64.StdEncoding.EncodeToString(cosyKey)
	payloadJSON, _ := json.Marshal(map[string]string{"version": "v1", "requestId": qoderUUID(), "info": infoB64, "cosyVersion": qoderCOSYVersion, "ideVersion": ""})
	payloadB64 := base64.StdEncoding.EncodeToString(payloadJSON)
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	sigPath := strings.TrimPrefix(u.Path, "/algo")
	timestamp := fmt.Sprint(time.Now().Unix())
	sigInput := make([]byte, 0, len(payloadB64)+len(cosyKeyB64)+len(timestamp)+len(body)+len(sigPath)+4)
	sigInput = append(sigInput, payloadB64...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, cosyKeyB64...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, timestamp...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, body...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, sigPath...)
	signature := md5.Sum(sigInput)
	bodyHash := md5.Sum(body)
	machineID := auth.machineID
	if machineID == "" {
		machineID = qoderUUID()
	}
	payload := "COSY." + payloadB64 + "." + hex.EncodeToString(signature[:])
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+payload)
	h.Set("Cosy-Key", cosyKeyB64)
	h.Set("Cosy-User", auth.userID)
	h.Set("Cosy-Date", timestamp)
	h.Set("Cosy-Version", qoderCOSYVersion)
	h.Set("Cosy-Machineid", machineID)
	h.Set("Cosy-Machinetoken", machineID)
	h.Set("Cosy-Machinetype", "5")
	h.Set("Cosy-Machineos", "x86_64_windows")
	h.Set("Cosy-Clienttype", "5")
	h.Set("Cosy-Clientip", "127.0.0.1")
	h.Set("Cosy-Bodyhash", hex.EncodeToString(bodyHash[:]))
	h.Set("Cosy-Bodylength", fmt.Sprint(len(body)))
	h.Set("Cosy-Sigpath", sigPath)
	h.Set("Cosy-Data-Policy", "disagree")
	h.Set("Cosy-Organization-Id", "")
	h.Set("Cosy-Organization-Tags", "")
	h.Set("Login-Version", "v2")
	h.Set("X-Request-Id", qoderUUID())
	return h, nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

func qoderBuildPayload(modelKey string, modelConfig map[string]any, requestBody []byte, userID string) ([]byte, error) {
	var incoming map[string]any
	if err := json.Unmarshal(requestBody, &incoming); err != nil {
		return nil, errors.New("invalid OpenAI request body")
	}
	messages, _ := incoming["messages"].([]any)
	cleanMessages := make([]map[string]any, 0, len(messages))
	var systems []string
	lastUser := ""
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		content := qoderTextContent(message["content"])
		if role == "system" {
			if content != "" {
				systems = append(systems, content)
			}
			continue
		}
		clean := make(map[string]any, len(message))
		for key, value := range message {
			if key != "content" {
				clean[key] = value
			}
		}
		clean["content"] = content
		cleanMessages = append(cleanMessages, clean)
		if role == "user" {
			lastUser = content
		}
	}
	tools, _ := incoming["tools"].([]any)
	if tools == nil {
		tools = []any{}
	}
	maxTokens := 32768.0
	if output, ok := modelConfig["max_output_tokens"].(float64); ok && output > 0 {
		maxTokens = output
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if value, ok := incoming[field].(float64); ok && value > 0 && value < maxTokens {
			maxTokens = value
		}
	}
	serializedMessages, _ := json.Marshal(cleanMessages)
	serializedTools, _ := json.Marshal(tools)
	recordID := qoderStableHash(modelKey, string(serializedMessages), string(serializedTools), fmt.Sprint(maxTokens))
	modelConfig = cloneQoderModelConfig(modelConfig)
	modelConfig["key"] = modelKey
	lastUserTitle := []rune(lastUser)
	if len(lastUserTitle) > 30 {
		lastUserTitle = lastUserTitle[:30]
	}
	var isReasoning bool
	isReasoning, _ = modelConfig["is_reasoning"].(bool)
	payload := map[string]any{
		"request_id": qoderUUID(), "request_set_id": recordID, "chat_record_id": recordID,
		"session_id": qoderStableHash("qoder-session", userID, modelKey), "stream": true,
		"chat_task": "FREE_INPUT", "is_reply": true, "is_retry": false, "source": 1,
		"version": "3", "session_type": "qodercli", "agent_id": "agent_common", "task_id": "common",
		"code_language": "", "chat_prompt": "", "image_urls": nil, "aliyun_user_type": "",
		"system": strings.Join(systems, "\n\n"), "messages": cleanMessages, "tools": tools,
		"parameters": map[string]any{"max_tokens": maxTokens}, "model_config": modelConfig,
		"chat_context": map[string]any{"chatPrompt": "", "imageUrls": nil, "extra": map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": modelKey, "is_reasoning": isReasoning}, "originalContent": lastUser}, "features": []any{}, "text": lastUser},
		"business":     map[string]any{"product": "cli", "version": "1.0.0", "type": "agent", "stage": "start", "id": qoderUUID(), "name": string(lastUserTitle), "begin_at": time.Now().UnixMilli()},
	}
	return json.Marshal(payload)
}

func cloneQoderModelConfig(config map[string]any) map[string]any {
	copy := make(map[string]any, len(config)+1)
	for key, value := range config {
		copy[key] = value
	}
	return copy
}

func qoderTextContent(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, text)
				} else if block["type"] == "image_url" {
					// shortcut: Qoder image URLs are reduced to a marker; add OSS uploads when multimodal relay is required.
					parts = append(parts, "[image omitted]")
				}
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (h *Handler) upstreamCallQoder(ctx context.Context, client *http.Client, token string, extra map[string]string, provider *model.Provider, modelKey string, body []byte, stream bool, entry string, w http.ResponseWriter, ua string) (int, UsageRecord, error) {
	region := qoderRegion(provider)
	auth, err := h.resolveQoderAuth(ctx, client, token, extra, region)
	if err != nil {
		return http.StatusUnauthorized, UsageRecord{}, err
	}
	modelConfig, err := h.qoderModelConfig(ctx, client, auth, region, modelKey)
	if err != nil {
		return 0, UsageRecord{}, err
	}
	plain, err := qoderBuildPayload(modelKey, modelConfig, body, auth.userID)
	if err != nil {
		return http.StatusBadRequest, UsageRecord{}, &UpstreamError{Status: http.StatusBadRequest, Body: err.Error(), ContentType: "text/plain"}
	}
	encoded := qoderEncodeBody(plain)
	target := qoderChatURL(region, auth.token)
	headers, err := qoderCosyHeaders(encoded, target, auth)
	if err != nil {
		return http.StatusUnauthorized, UsageRecord{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return 0, UsageRecord{}, err
	}
	req.Header = headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-Model-Key", modelKey)
	if source, ok := modelConfig["source"].(string); ok && source != "" {
		req.Header.Set("X-Model-Source", source)
	} else {
		req.Header.Set("X-Model-Source", "system")
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	} else {
		req.Header.Set("User-Agent", "qodercli/1.0.0")
	}
	upResp, err := client.Do(req)
	if err != nil {
		return 0, UsageRecord{}, err
	}
	defer upResp.Body.Close()
	if upResp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(upResp.Body, 4<<10))
		return upResp.StatusCode, UsageRecord{}, &UpstreamError{Status: upResp.StatusCode, Body: string(b), ContentType: upResp.Header.Get("Content-Type")}
	}
	if stream {
		innerBody, status, err := qoderUnwrapSSE(upResp.Body)
		if err != nil {
			return status, UsageRecord{}, err
		}
		defer innerBody.Close()
		innerResp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: innerBody}
		if entry == protoOpenAI {
			return streamPassthrough(w, innerResp)
		}
		return bridgeChatChunksToEntry(w, innerResp, entry, modelKey)
	}
	completion, status, err := qoderCollectCompletion(upResp.Body, modelKey)
	if err != nil {
		return status, UsageRecord{}, err
	}
	if entry != protoOpenAI {
		agg, ok := openaiJSONToAgg(completion)
		if !ok {
			return http.StatusBadGateway, UsageRecord{}, errors.New("qoder returned an invalid completion")
		}
		if agg.ErrMsg != "" && agg.Content == "" && len(agg.ToolCalls) == 0 {
			return status, UsageRecord{}, errors.New("qoder: " + agg.ErrMsg)
		}
		usage := UsageRecord{PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens, Found: agg.PromptTokens > 0 || agg.CompletionTokens > 0}
		writeEntryJSON(w, entry, modelKey, agg)
		return status, usage, nil
	}
	usage := extractUsageFromJSON(completion)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(completion)
	return http.StatusOK, usage, nil
}

type QoderProbeResult struct {
	Status           int
	LatencyMS        int64
	Content          string
	Reasoning        string
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
	Error            string
}

// QoderProbe 用真实 COSY relay 路径执行渠道测活，结果只返回调用该接口的用户。
func QoderProbe(ctx context.Context, client *http.Client, token string, extra map[string]string, provider *model.Provider, modelKey, prompt string, maxTokens int, stream bool) QoderProbeResult {
	start := time.Now()
	body, _ := json.Marshal(map[string]any{"model": modelKey, "max_tokens": maxTokens, "messages": []any{map[string]string{"role": "user", "content": prompt}}, "stream": stream})
	w := httptest.NewRecorder()
	result := QoderProbeResult{}
	var callErr error
	result.Status, _, callErr = (&Handler{}).upstreamCallQoder(ctx, client, token, extra, provider, modelKey, body, stream, protoOpenAI, w, "")
	result.LatencyMS = time.Since(start).Milliseconds()
	if callErr != nil {
		result.Error = callErr.Error()
		var upstream *UpstreamError
		if errors.As(callErr, &upstream) {
			result.Status = upstream.Status
			if detail := sanitizeErrMessage(upstream.Body, upstream.ContentType); detail != "" {
				result.Error += ": " + detail
			}
		}
		return result
	}
	if result.Status >= 400 {
		result.Error = fmt.Sprintf("upstream returned %d", result.Status)
		return result
	}
	if stream {
		scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
		for scanner.Scan() {
			event, ok := parseSSELine(scanner.Bytes())
			if !ok {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					Prompt     int `json:"prompt_tokens"`
					Completion int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(event, &chunk) != nil {
				continue
			}
			for _, choice := range chunk.Choices {
				result.Content += choice.Delta.Content
				result.Reasoning += choice.Delta.ReasoningContent
				if choice.FinishReason != nil {
					result.FinishReason = *choice.FinishReason
				}
			}
			if chunk.Usage != nil {
				result.PromptTokens = chunk.Usage.Prompt
				result.CompletionTokens = chunk.Usage.Completion
			}
		}
		if err := scanner.Err(); err != nil {
			result.Error = "read Qoder stream: " + err.Error()
		}
		return result
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &completion); err != nil || len(completion.Choices) == 0 {
		result.Error = "Qoder returned an invalid completion"
		return result
	}
	result.Content = completion.Choices[0].Message.Content
	result.Reasoning = completion.Choices[0].Message.ReasoningContent
	result.FinishReason = completion.Choices[0].FinishReason
	result.PromptTokens = completion.Usage.Prompt
	result.CompletionTokens = completion.Usage.Completion
	return result
}

func qoderDecodeEnvelope(line []byte) (string, int, error) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return "", 0, nil
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return "[DONE]", 200, nil
	}
	var envelope struct {
		Status any `json:"statusCodeValue"`
		Body   any `json:"body"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", 0, nil
	}
	status := http.StatusOK
	switch value := envelope.Status.(type) {
	case float64:
		if value > 0 {
			status = int(value)
		}
	case string:
		var n int
		_, _ = fmt.Sscan(value, &n)
		if n > 0 {
			status = n
		}
	}
	var inner string
	switch value := envelope.Body.(type) {
	case string:
		inner = value
	case nil:
	default:
		b, _ := json.Marshal(value)
		inner = string(b)
	}
	return inner, status, nil
}

func qoderUnwrapSSE(body io.ReadCloser) (io.ReadCloser, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 16*1024), 2<<20)
	var first string
	for scanner.Scan() {
		inner, status, err := qoderDecodeEnvelope(scanner.Bytes())
		if err != nil {
			return nil, http.StatusBadGateway, err
		}
		if status == 0 {
			continue
		}
		if status != http.StatusOK {
			return nil, status, &UpstreamError{Status: status, Body: inner, ContentType: "application/json"}
		}
		if inner != "" {
			first = inner
			break
		}
	}
	if first == "" && scanner.Err() != nil {
		return nil, http.StatusBadGateway, scanner.Err()
	}
	if first == "" {
		return nil, http.StatusBadGateway, errors.New("qoder stream ended without a data frame")
	}
	reader, writer := io.Pipe()
	go func() {
		done := false
		defer func() {
			if done {
				_ = writer.Close()
			} else {
				_ = writer.CloseWithError(io.ErrUnexpectedEOF)
			}
		}()
		writeInner := func(inner string) bool {
			if inner == "" {
				return true
			}
			if inner != "[DONE]" {
				inner = qoderNormalizeChunk(inner)
			}
			_, err := io.WriteString(writer, "data: "+inner+"\n\n")
			return err == nil
		}
		if !writeInner(first) {
			return
		}
		if first == "[DONE]" {
			done = true
			return
		}
		for scanner.Scan() {
			inner, status, err := qoderDecodeEnvelope(scanner.Bytes())
			if err != nil {
				return
			}
			if status == 0 {
				continue
			}
			if status != http.StatusOK {
				data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": inner, "code": status}})
				_ = writeInner(string(data))
				_ = writeInner("[DONE]")
				done = true
				return
			}
			if !writeInner(inner) {
				return
			}
			if inner == "[DONE]" {
				done = true
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = writer.CloseWithError(err)
			done = true
		}
	}()
	return reader, http.StatusOK, nil
}

func qoderNormalizeChunk(raw string) string {
	var event map[string]any
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return raw
	}
	choices, _ := event["choices"].([]any)
	changed := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if finish, ok := delta["finish_reason"]; ok {
			choice["finish_reason"] = finish
			delete(delta, "finish_reason")
			changed = true
		}
	}
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return raw
	}
	return string(encoded)
}

type qoderToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Index    int    `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func qoderCollectCompletion(body io.Reader, modelKey string) ([]byte, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 16*1024), 2<<20)
	content, reasoning := strings.Builder{}, strings.Builder{}
	toolCalls := map[int]*qoderToolCall{}
	finish := "stop"
	promptTokens, completionTokens := 0, 0
	var completionID string
	status := http.StatusOK
	done := false
	for scanner.Scan() {
		inner, frameStatus, err := qoderDecodeEnvelope(scanner.Bytes())
		if err != nil {
			return nil, http.StatusBadGateway, err
		}
		if frameStatus == 0 {
			continue
		}
		if frameStatus != http.StatusOK {
			return nil, frameStatus, &UpstreamError{Status: frameStatus, Body: inner, ContentType: "application/json"}
		}
		if inner == "" || inner == "[DONE]" {
			if inner == "[DONE]" {
				done = true
				break
			}
			continue
		}
		var event struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content          string  `json:"content"`
					ReasoningContent string  `json:"reasoning_content"`
					FinishReason     *string `json:"finish_reason"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(inner), &event); err != nil {
			continue
		}
		if completionID == "" {
			completionID = event.ID
		}
		for _, choice := range event.Choices {
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			content.WriteString(choice.Message.Content)
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
			if choice.Delta.FinishReason != nil {
				finish = *choice.Delta.FinishReason
			}
			for _, call := range choice.Delta.ToolCalls {
				acc := toolCalls[call.Index]
				if acc == nil {
					acc = &qoderToolCall{Type: "function", Index: call.Index}
					toolCalls[call.Index] = acc
				}
				if call.ID != "" {
					acc.ID = call.ID
				}
				if call.Type != "" {
					acc.Type = call.Type
				}
				acc.Function.Name += call.Function.Name
				acc.Function.Arguments += call.Function.Arguments
			}
		}
		if event.Usage != nil {
			promptTokens = event.Usage.Prompt
			completionTokens = event.Usage.Completion
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, status, err
	}
	if !done {
		return nil, status, io.ErrUnexpectedEOF
	}
	orderedTools := make([]qoderToolCall, 0, len(toolCalls))
	indexes := make([]int, 0, len(toolCalls))
	for index := range toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		if call := toolCalls[index]; call != nil {
			orderedTools = append(orderedTools, *call)
		}
	}
	if completionID == "" {
		completionID = "chatcmpl-" + qoderUUID()
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(orderedTools) > 0 {
		message["tool_calls"] = orderedTools
	}
	completion := map[string]any{
		"id": completionID, "object": "chat.completion", "created": time.Now().Unix(), "model": modelKey,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens},
	}
	encoded, err := json.Marshal(completion)
	return encoded, http.StatusOK, err
}
