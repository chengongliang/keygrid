package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cosy.go Qoder COSY 签名（RSA + AES-128-CBC + MD5），移植自 9router
// open-sse/shared/qoder/cosy.js（源自 CLIProxyAPIPlus internal/auth/qoder/cosy.go）。
//
// 每个签名请求都带：
//   - AES-128-CBC 加密的用户信息（uid + oauth token + 姓名/邮箱），AES key 用 RSA 公钥包裹；
//   - 对 `payload || cosyKey || timestamp || body || sigPath` 的 MD5 签名；
//   - 正文的 MD5 + 长度，供上游校验完整性；
//   - 一组 Cosy-* / X-* 客户端指纹头（machineId、IDE 版本等）。
//
// 头名大小写与 qodercli 一致（Cosy-Machineid 而非 Cosy-MachineID）。
// Go 的 http.Header 会规范化成 Cosy-Machineid，与之一致。

// Creds COSY 签名所需的账号信息。
type Creds struct {
	UserID    string // Qoder 用户 id（userinfo 拿，签名必需）
	Token     string // 设备流 dt-… / job token jt-…
	Name      string
	Email     string
	MachineID string // 持久化的机器 UUID（空则每次随机）
}

// signKey RSA 公钥（进程内解析一次）。
var signKey = func() *rsa.PublicKey {
	block, _ := pem.Decode([]byte(rsaPublicKey))
	if block == nil {
		return nil
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil
	}
	pub, _ := key.(*rsa.PublicKey)
	return pub
}()

// ErrMissingUserID 凭据缺少 userId（COSY 无法签名）。
var ErrMissingUserID = errors.New("qoder cosy: user id is empty")

// ErrMissingToken 凭据缺少 access token。
var ErrMissingToken = errors.New("qoder cosy: auth token is empty")

// BuildHeaders 构造一次 Qoder 请求的完整 COSY 头。
//
// body 必须与实际发出的字节完全一致（对话请求是编码后的正文），
// rawURL 用于推导 Cosy-Sigpath（去掉前导 /algo）。
func BuildHeaders(body []byte, rawURL string, c Creds) (http.Header, error) {
	if c.UserID == "" {
		return nil, ErrMissingUserID
	}
	if c.Token == "" {
		return nil, ErrMissingToken
	}
	if signKey == nil {
		return nil, errors.New("qoder cosy: rsa public key unavailable")
	}

	aesKey := newAESKey()
	infoB64, cosyKeyB64, err := encryptUserInfo(aesKey, userInfo{
		UID:                c.UserID,
		SecurityOAuthToken: c.Token,
		Name:               c.Name,
		AID:                "",
		Email:              c.Email,
	})
	if err != nil {
		return nil, err
	}

	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	requestID := UUID()

	payload := fmt.Sprintf(`{"version":"v1","requestId":%s,"info":%s,"cosyVersion":%s,"ideVersion":""}`,
		jsonString(requestID), jsonString(infoB64), jsonString(IDEVersion))
	payloadB64 := base64.StdEncoding.EncodeToString([]byte(payload))

	sigPath := sigPathOf(rawURL)
	sigInput := payloadB64 + "\n" + cosyKeyB64 + "\n" + timestamp + "\n" + string(body) + "\n" + sigPath
	sig := md5Hex([]byte(sigInput))

	machineID := c.MachineID
	if machineID == "" {
		machineID = UUID()
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer COSY."+payloadB64+"."+sig)
	h.Set("Cosy-Key", cosyKeyB64)
	h.Set("Cosy-User", c.UserID)
	h.Set("Cosy-Date", timestamp)
	h.Set("Cosy-Version", IDEVersion)
	h.Set("Cosy-Machineid", machineID)
	h.Set("Cosy-Machinetoken", machineID)
	h.Set("Cosy-Machinetype", MachineType)
	h.Set("Cosy-Machineos", MachineOS)
	h.Set("Cosy-Clienttype", ClientType)
	h.Set("Cosy-Clientip", "127.0.0.1")
	h.Set("Cosy-Bodyhash", md5Hex(body))
	h.Set("Cosy-Bodylength", fmt.Sprintf("%d", len(body)))
	h.Set("Cosy-Sigpath", sigPath)
	h.Set("Cosy-Data-Policy", DataPolicy)
	h.Set("Cosy-Organization-Id", "")
	h.Set("Cosy-Organization-Tags", "")
	h.Set("Login-Version", LoginVersion)
	h.Set("X-Request-Id", UUID())
	return h, nil
}

// ApplyHeaders 把 COSY 头写进 req（不覆盖调用方随后要设置的基础头）。
func ApplyHeaders(req *http.Request, body []byte, rawURL string, c Creds) error {
	h, err := BuildHeaders(body, rawURL, c)
	if err != nil {
		return err
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	return nil
}

// userInfo AES 加密的明文结构（字段名与 qodercli 一致，不可改）。
type userInfo struct {
	UID                string `json:"uid"`
	SecurityOAuthToken string `json:"security_oauth_token"`
	Name               string `json:"name"`
	AID                string `json:"aid"`
	Email              string `json:"email"`
}

// newAESKey 生成 16 字符 AES key：取新 UUID 规范串前 16 字符（含连字符），
// 与 qodercli/Veria 一致。
func newAESKey() string { return UUID()[:16] }

// encryptUserInfo AES-128-CBC 加密用户信息 + RSA 包裹 AES key。
func encryptUserInfo(aesKey string, info userInfo) (infoB64, keyB64 string, err error) {
	plain, err := jsonMarshal(info)
	if err != nil {
		return "", "", err
	}
	infoB64, err = aesEncryptCBC(plain, aesKey)
	if err != nil {
		return "", "", err
	}
	keyB64, err = rsaEncrypt(aesKey)
	if err != nil {
		return "", "", err
	}
	return infoB64, keyB64, nil
}

// aesEncryptCBC AES-128-CBC + PKCS7（固定添加整块填充，与 JS 实现一致）。
// IV 复用 key 字节：key 每请求新鲜，因此 IV 也随请求变化。
func aesEncryptCBC(plain []byte, key string) (string, error) {
	keyBytes := []byte(key)
	if len(keyBytes) != 16 {
		return "", fmt.Errorf("qoder cosy: aes key must be 16 bytes, got %d", len(keyBytes))
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plain, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, keyBytes[:aes.BlockSize]).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// pkcs7Pad 始终填充：长度恰好为块整数倍时补一整块（与 JS 端一致）。
func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+padding)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(padding)
	}
	return out
}

// rsaEncrypt RSA PKCS#1 v1.5 加密（对应 JS 的 RSA_PKCS1_PADDING）。
func rsaEncrypt(data string) (string, error) {
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, signKey, []byte(data))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// sigPathOf 取 URL path 并剥掉前导 /algo（qodercli 约定）。
func sigPathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	p := u.Path
	if strings.HasPrefix(p, "/algo") {
		return strings.TrimPrefix(p, "/algo")
	}
	return p
}

// md5Hex 十六进制小写 MD5。
func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// sha256Hex 十六进制小写 SHA-256（缓存键 / 稳定 id 用）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// jsonString JSON 字符串字面量（只用于拼 COSY payload，避免整包 marshal 的结构差异）。
func jsonString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
