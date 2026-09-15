package crypto

import (
	"crypto/sha256"
	"log"
)

// InitFromSecret 从 MASTER_KEY env 派生 32 字节 AES key。
// 允许任意长度口令，内部 sha256 归一化。
func InitFromSecret(secret string) {
	if secret == "" {
		log.Fatal("MASTER_KEY is required (used to encrypt credentials)")
	}
	sum := sha256.Sum256([]byte(secret))
	Init(sum[:])
}
