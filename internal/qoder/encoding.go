package qoder

import "encoding/base64"

// encoding.go Qoder 正文编码（WAF 混淆），移植自 9router
// open-sse/shared/qoder/encoding.js（源自 qoder2api 的 QoderEncoding.java）。
//
// 算法：
//  1. 标准 base64 编码明文；
//  2. 结果切成三等份后重排为 [尾][中][头]；
//  3. 逐字符按自定义字母表替换（"=" → "$"）。
//
// 编码后的正文必须搭配 URL 上的 &Encode=1 发送，上游按逆序解码。
// 目的是绕开阿里云 WAF 对明文请求体的特征匹配。

const (
	// stdAlphabet 标准 base64 字母表。
	stdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	// customAlphabet Qoder 替换字母表（64 个字符，与标准表逐位对应）。
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
)

// s2c 标准字母 → 自定义字母映射（ASCII 表，-1 表示不替换）。
var s2c = func() [128]int16 {
	var t [128]int16
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(stdAlphabet); i++ {
		t[stdAlphabet[i]] = int16(customAlphabet[i])
	}
	t['='] = int16('$')
	return t
}()

// EncodeBody 按 Qoder 约定编码请求体，返回可直接作为 HTTP body 的字节串
// （输出全部是 ASCII 可见字符）。
func EncodeBody(plain []byte) []byte {
	if len(plain) == 0 {
		return []byte{}
	}
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	// [尾][中][头]
	rearranged := std[n-a:] + std[a:n-a] + std[:a]

	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if int(c) < len(s2c) && s2c[c] >= 0 {
			out[i] = byte(s2c[c])
		} else {
			out[i] = c
		}
	}
	return out
}
