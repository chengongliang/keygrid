package oidc

// PKCE S256（RFC 7636）+ nonce/state 随机生成。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// RandomBase64URL n 字节随机 → base64url（无 padding）。state/nonce/verifier 用。
func RandomBase64URL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomHex n 字节随机 → hex。
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewCodeVerifier PKCE verifier（43-128 chars，unreserved）。64 字节 → 86 chars。
func NewCodeVerifier() string { return RandomBase64URL(64) }

// S256Challenge verifier → code_challenge = BASE64URL(SHA256(verifier))。
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
