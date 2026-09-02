package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Listen         string
	BaseURL        string
	APIAuthKey     string
	BinanceKey     string
	BinanceSecret  string
	BinanceUID     string
	BinanceAPIBase string
	QRImage        string
	ReceiveEmail   string
	Currencies     map[string]bool
	OrderTTL       int64 // 秒
	ExpiredGrace   int64 // 秒
	SuffixCooldown int64 // 秒
	PollInterval   int64 // 秒
	AmountDecimals int
	SuffixMode     string
	DBPath         string
	LogLevel       string
}

func loadConfig(path string) (*Config, error) {
	vals := map[string]string{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			kv := strings.SplitN(line, "=", 2)
			if len(kv) != 2 {
				continue
			}
			vals[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	get := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v
		}
		if v, ok := vals[k]; ok && v != "" {
			return v
		}
		return def
	}
	getInt := func(k string, def int64) int64 {
		s := get(k, "")
		if s == "" {
			return def
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return def
		}
		return n
	}
	c := &Config{
		Listen:         get("LISTEN", "0.0.0.0:8080"),
		BaseURL:        strings.TrimRight(get("BASE_URL", "http://127.0.0.1:8080"), "/"),
		APIAuthKey:     get("API_AUTH_KEY", ""),
		BinanceKey:     get("BINANCE_API_KEY", ""),
		BinanceSecret:  get("BINANCE_API_SECRET", ""),
		BinanceUID:     get("BINANCE_UID", ""),
		BinanceAPIBase: strings.TrimRight(get("BINANCE_API_BASE", "https://api.binance.com"), "/"),
		QRImage:        get("QR_IMAGE", ""),
		ReceiveEmail:   get("RECEIVE_EMAIL", ""),
		Currencies:     map[string]bool{},
		OrderTTL:       getInt("ORDER_TTL", 900),
		ExpiredGrace:   getInt("EXPIRED_GRACE", 1800),
		SuffixCooldown: getInt("SUFFIX_COOLDOWN", 86400),
		PollInterval:   getInt("POLL_INTERVAL", 4),
		AmountDecimals: int(getInt("AMOUNT_DECIMALS", 4)),
		SuffixMode:     get("SUFFIX_MODE", "add"),
		DBPath:         get("DB_PATH", "./bpaygate.db"),
		LogLevel:       strings.ToLower(get("LOG_LEVEL", "info")),
	}
	for _, cur := range strings.Split(get("CURRENCIES", "USDT,USDC"), ",") {
		cur = strings.ToUpper(strings.TrimSpace(cur))
		if cur != "" {
			c.Currencies[cur] = true
		}
	}
	if len(c.APIAuthKey) < 16 {
		return nil, errors.New("API_AUTH_KEY 未设置或过短（≥16 字符，可用 ./bpaygate -gen-key 生成）")
	}
	if c.BinanceKey == "" || c.BinanceSecret == "" {
		return nil, errors.New("BINANCE_API_KEY / BINANCE_API_SECRET 未设置（只读权限即可）")
	}
	if c.BinanceUID == "" {
		return nil, errors.New("BINANCE_UID 未设置")
	}
	if c.AmountDecimals < 2 || c.AmountDecimals > 8 {
		return nil, errors.New("AMOUNT_DECIMALS 须在 2~8 之间")
	}
	if c.SuffixMode != "add" && c.SuffixMode != "sub" {
		return nil, errors.New("SUFFIX_MODE 只能是 add 或 sub")
	}
	if c.PollInterval < 1 {
		c.PollInterval = 1
	}
	if c.QRImage != "" {
		if _, err := os.Stat(c.QRImage); err != nil {
			return nil, fmt.Errorf("QR_IMAGE 文件不存在: %s", c.QRImage)
		}
	}
	return c, nil
}
