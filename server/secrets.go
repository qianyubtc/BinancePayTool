package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// deriveKey 由 API_AUTH_KEY 派生 AES-256 密钥，用于加密动态添加的收款账号的 API Secret。
// 改动 API_AUTH_KEY 会导致已存账号的密钥无法解密（需重新添加账号）。
func deriveKey(secret string) []byte {
	h := sha256.Sum256([]byte("bpaygate-accounts:" + secret))
	return h[:]
}

func encryptStr(key []byte, plain string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plain), nil)
	return "v1:" + base64.RawStdEncoding.EncodeToString(append(nonce, ct...)), nil
}

func decryptStr(key []byte, enc string) (string, error) {
	if !strings.HasPrefix(enc, "v1:") {
		return "", errors.New("未知密文格式")
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc[3:])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("密文过短")
	}
	pt, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
