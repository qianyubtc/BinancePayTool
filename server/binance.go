package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
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

	mu          sync.Mutex
	clockSkewMs int64 // 币安服务器时间 - 本地时间；命中 -1021 后由 /api/v3/time 校准
}

func newBinanceClient(cfg *Config) *binanceClient {
	return newBinanceClientFor(cfg.BinanceAPIBase, cfg.BinanceKey, cfg.BinanceSecret)
}

func newBinanceClientFor(base, key, secret string) *binanceClient {
	return &binanceClient{base: base, key: key, secret: secret, hc: &http.Client{Timeout: 15 * time.Second}}
}

// PayTransactions 拉取 Pay 流水（签名 GET）。limit 上限 100，接口权重 3000/UID。
// 本地时钟偏差触发币安 -1021 时，自动按 /api/v3/time 校准并重试一次。
func (b *binanceClient) PayTransactions(startMs, endMs int64, limit int) ([]payTxn, error) {
	txns, err := b.payTransactions(startMs, endMs, limit)
	var be *binanceError
	if !errors.As(err, &be) || be.code != -1021 {
		return txns, err
	}
	if syncErr := b.synchronizeClock(); syncErr != nil {
		return nil, err
	}
	return b.payTransactions(startMs, endMs, limit)
}

func (b *binanceClient) payTransactions(startMs, endMs int64, limit int) ([]payTxn, error) {
	q := url.Values{}
	q.Set("timestamp", strconv.FormatInt(nowMs()+b.clockSkew(), 10))
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
		// 币安业务错误体带 code/msg（如 -1021 时间戳偏差、-2015 Key 无效），保留错误码供上层识别。
		var e struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(body, &e) == nil && e.Code != 0 {
			return nil, &binanceError{status: resp.StatusCode, code: e.Code, msg: e.Msg}
		}
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

// binanceError 保留币安业务错误码，供调用方识别 -1021（时间戳偏差）等可恢复错误。
type binanceError struct {
	status int
	code   int
	msg    string
}

func (e *binanceError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("binance HTTP %d code=%d", e.status, e.code)
	}
	return fmt.Sprintf("binance HTTP %d code=%d: %s", e.status, e.code, truncate(e.msg, 300))
}

// synchronizeClock 用 GET /api/v3/time 校准本地与币安服务器的时间差。
// 取请求往返的中值，避免把网络延迟整体记成时钟偏差。
func (b *binanceClient) synchronizeClock() error {
	startedAt := nowMs()
	resp, err := b.hc.Get(b.base + "/api/v3/time")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("binance time HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var r struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("binance time 响应解析失败: %w", err)
	}
	if r.ServerTime <= 0 {
		return errors.New("binance time 响应缺少 serverTime")
	}
	completedAt := nowMs()
	b.mu.Lock()
	b.clockSkewMs = r.ServerTime - (startedAt+completedAt)/2
	b.mu.Unlock()
	return nil
}

func (b *binanceClient) clockSkew() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clockSkewMs
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
