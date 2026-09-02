package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacHex(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// signRequest 商户→网关请求签名（见 docs/protocol.md §1.1）
func signRequest(secret, ts, nonce, method, path string, body []byte) string {
	return hmacHex(secret, ts+"\n"+nonce+"\n"+method+"\n"+path+"\n"+sha256Hex(body))
}

// signCallback 网关→商户回调签名（见 docs/protocol.md §1.2）
func signCallback(secret, ts, nonce string, body []byte) string {
	return hmacHex(secret, ts+"\n"+nonce+"\n"+sha256Hex(body))
}

func sigEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }
