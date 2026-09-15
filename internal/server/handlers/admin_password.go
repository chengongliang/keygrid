package handlers

import (
	"crypto/rand"
	"math/big"
)

// genPassword 生成一次性随机密码（16 位字母数字，无易混淆字符）。
// 无小写 l/o/1 与数字 0/1，重置密码可读性更好。
func genPassword() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, 16)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}
