package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// payTxn 对应 GET /sapi/v1/pay/transactions 返回的一条流水。
// 字段以 2026-09-02 真实账号实测为准（文档缺 orderId/note/counterpartyId，实测存在）。
type payTxn struct {
	OrderID        string `json:"orderId"`       // 18 位数字，付款方 App 显示的「订单编号」
	Note           string `json:"note"`          // 付款方备注，可能缺省
	OrderType      string `json:"orderType"`     // C2C / PAYOUT / ...
	TransactionID  string `json:"transactionId"` // P_xxxxxxxxxxxxxxxx
	Time           int64  `json:"transactionTime"`
	Amount         string `json:"amount"` // 收款为正、付款为负
	Currency       string `json:"currency"`
	CounterpartyID int64  `json:"counterpartyId"`
	PayerInfo      struct {
		Name      string `json:"name"`
		BinanceID int64  `json:"binanceId"`
	} `json:"payerInfo"`
}

type binanceClient struct {
	base   string
	key    string
	secret string
	hc     *http.Client
}

func newBinanceClient(cfg *Config) *binanceClient {
	return newBinanceClientFor(cfg.BinanceAPIBase, cfg.BinanceKey, cfg.BinanceSecret)
}

func newBinanceClientFor(base, key, secret string) *binanceClient {
	return &binanceClient{base: base, key: key, secret: secret, hc: &http.Client{Timeout: 15 * time.Second}}
}

// PayTransactions 拉取 Pay 流水（签名 GET）。limit 上限 100，接口权重 3000/UID。
func (b *binanceClient) PayTransactions(startMs, endMs int64, limit int) ([]payTxn, error) {
	q := url.Values{}
	q.Set("timestamp", strconv.FormatInt(nowMs(), 10))
	q.Set("recvWindow", "10000")
	q.Set("limit", strconv.Itoa(limit))
	if startMs > 0 {
		q.Set("startTime", strconv.FormatInt(startMs, 10))
	}
	if endMs > 0 {
		q.Set("endTime", strconv.FormatInt(endMs, 10))
	}
	enc := q.Encode()
	sig := hmacHex(b.secret, enc)
	req, err := http.NewRequest("GET", b.base+"/sapi/v1/pay/transactions?"+enc+"&signature="+sig, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", b.key)
	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("binance HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var env struct {
		Data    []payTxn `json:"data"`
		Success bool     `json:"success"`
		Message string   `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("binance 响应解析失败: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("binance 返回失败: %s", truncate(string(body), 300))
	}
	return env.Data, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
