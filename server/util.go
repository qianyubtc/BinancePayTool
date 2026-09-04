package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

const scale = int64(100000000) // 1e8，金额内部一律用 1e-8 定点整数，避免浮点误差

func nowMs() int64 { return time.Now().UnixMilli() }

// parseAmount 把十进制字符串解析为 1e-8 定点整数。拒绝科学计数法、超 8 位小数、非法字符。
func parseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("empty amount")
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(frac) > 8 {
		return 0, errors.New("more than 8 decimals")
	}
	for _, c := range intPart + frac {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid amount")
		}
	}
	var v int64
	for _, c := range intPart {
		v = v*10 + int64(c-'0')
		if v > (int64(1)<<62)/scale {
			return 0, errors.New("amount too large")
		}
	}
	v *= scale
	f := int64(0)
	for _, c := range frac {
		f = f*10 + int64(c-'0')
	}
	for i := len(frac); i < 8; i++ {
		f *= 10
	}
	v += f
	if neg {
		v = -v
	}
	return v, nil
}

// fmtAmount 定点整数转字符串，去掉小数尾零。
func fmtAmount(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	ip := v / scale
	fp := v % scale
	s := fmt.Sprintf("%d", ip)
	if fp > 0 {
		fs := strings.TrimRight(fmt.Sprintf("%08d", fp), "0")
		s = s + "." + fs
	}
	if neg {
		s = "-" + s
	}
	return s
}

// decimalsOf 返回该定点金额实际用到的小数位数。
func decimalsOf(v int64) int {
	if v < 0 {
		v = -v
	}
	fp := v % scale
	if fp == 0 {
		return 0
	}
	d := 8
	for fp%10 == 0 {
		fp /= 10
		d--
	}
	return d
}

const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// codeAlphabet 去掉了易混淆的 0O1IL
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func randString(alphabet string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	out := make([]byte, n)
	for i, c := range b {
		out[i] = alphabet[int(c)%len(alphabet)]
	}
	return string(out)
}

func newOrderID() string   { return "o" + randString(idAlphabet, 25) }
func newAccountID() string { return "a" + randString(idAlphabet, 20) }
func newToken() string     { return randString(idAlphabet, 26) }
func newNoteCode() string  { return randString(codeAlphabet, 6) }
func newNonce() string     { return randString(idAlphabet, 24) }
