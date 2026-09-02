// Package bpaygate 是 BinancePayTool 网关的 Go 接入 SDK（仅标准库）。
//
//	gw := bpaygate.New("https://pay.example.com", "你的API_AUTH_KEY")
//	order, err := gw.CreateOrder(bpaygate.CreateOrderReq{
//		MerchantOrderID: "SHOP-1001", Currency: "USDT", Amount: "10",
//		CallbackURL: "https://shop.example.com/bpg/notify",
//	})
//	// 跳转用户到 order.PayURL；回调用 VerifyCallback 验签后按 MerchantOrderID 幂等发货。
package bpaygate

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

type Client struct {
	BaseURL string
	Secret  string
	HC      *http.Client
}

func New(baseURL, secret string) *Client {
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	return &Client{BaseURL: baseURL, Secret: secret, HC: &http.Client{Timeout: 10 * time.Second}}
}

type Order struct {
	OrderID         string `json:"order_id"`
	MerchantOrderID string `json:"merchant_order_id"`
	Status          string `json:"status"`
	Currency        string `json:"currency"`
	BaseAmount      string `json:"base_amount"`
	PayAmount       string `json:"pay_amount"`
	ActualAmount    string `json:"actual_amount"`
	NoteCode        string `json:"note_code"`
	PayURL          string `json:"pay_url"`
	ReceiveUID      string `json:"receive_uid"`
	MatchedBy       string `json:"matched_by"`
	BinanceOrderID  string `json:"binance_order_id"`
	BinanceTxnID    string `json:"binance_txn_id"`
	PayerID         string `json:"payer_id"`
	Overpaid        bool   `json:"overpaid"`
	CreatedAt       int64  `json:"created_at"`
	ExpiresAt       int64  `json:"expires_at"`
	PaidAt          int64  `json:"paid_at"`
}

type Callback struct {
	Event           string `json:"event"` // paid | underpaid | expired
	OrderID         string `json:"order_id"`
	MerchantOrderID string `json:"merchant_order_id"`
	Status          string `json:"status"`
	Currency        string `json:"currency"`
	BaseAmount      string `json:"base_amount"`
	PayAmount       string `json:"pay_amount"`
	ActualAmount    string `json:"actual_amount"`
	Overpaid        bool   `json:"overpaid"`
	MatchedBy       string `json:"matched_by"`
	BinanceOrderID  string `json:"binance_order_id"`
	BinanceTxnID    string `json:"binance_txn_id"`
	PayerID         string `json:"payer_id"`
	PaidAt          int64  `json:"paid_at"`
	Timestamp       int64  `json:"timestamp"`
}

type APIError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

type CreateOrderReq struct {
	MerchantOrderID string `json:"merchant_order_id"`
	Currency        string `json:"currency"`
	Amount          string `json:"amount"`
	CallbackURL     string `json:"callback_url,omitempty"`
	ReturnURL       string `json:"return_url,omitempty"`
	Timeout         int64  `json:"timeout,omitempty"`
}

func (c *Client) CreateOrder(req CreateOrderReq) (*Order, error) {
	return c.do("POST", "/api/v1/orders", req)
}

func (c *Client) GetOrder(orderID string) (*Order, error) {
	return c.do("GET", "/api/v1/orders/"+orderID, nil)
}

func (c *Client) GetOrderByMerchantID(mid string) (*Order, error) {
	return c.do("GET", "/api/v1/orders/by-merchant/"+mid, nil)
}

func (c *Client) CloseOrder(orderID string) (*Order, error) {
	return c.do("POST", "/api/v1/orders/"+orderID+"/close", struct{}{})
}

func (c *Client) do(method, path string, payload any) (*Order, error) {
	var body []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = b
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce := randNonce()
	req, err := http.NewRequest(method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BPG-Timestamp", ts)
	req.Header.Set("X-BPG-Nonce", nonce)
	req.Header.Set("X-BPG-Signature", SignRequest(c.Secret, ts, nonce, method, path, body))
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var env struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("响应解析失败 (HTTP %d): %w", resp.StatusCode, err)
	}
	if env.Code != "OK" {
		return nil, &APIError{Code: env.Code, Message: env.Message, HTTPStatus: resp.StatusCode}
	}
	var o Order
	if err := json.Unmarshal(env.Data, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// VerifyCallback 校验网关回调签名并解析。商户须按 MerchantOrderID 幂等处理。
func VerifyCallback(header http.Header, body []byte, secret string, maxSkew time.Duration) (*Callback, error) {
	ts := header.Get("X-BPG-Timestamp")
	nonce := header.Get("X-BPG-Nonce")
	sig := header.Get("X-BPG-Signature")
	if ts == "" || nonce == "" || sig == "" {
		return nil, errors.New("缺少签名头")
	}
	tsv, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, errors.New("时间戳非法")
	}
	skew := time.Now().UnixMilli() - tsv
	if skew < 0 {
		skew = -skew
	}
	if maxSkew <= 0 {
		maxSkew = 5 * time.Minute
	}
	if skew > maxSkew.Milliseconds() {
		return nil, errors.New("时间戳偏差过大")
	}
	if !hmac.Equal([]byte(SignCallback(secret, ts, nonce, body)), []byte(sig)) {
		return nil, errors.New("签名错误")
	}
	var cb Callback
	if err := json.Unmarshal(body, &cb); err != nil {
		return nil, err
	}
	return &cb, nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacHex(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// SignRequest 商户→网关请求签名（docs/protocol.md §1.1）。
func SignRequest(secret, ts, nonce, method, path string, body []byte) string {
	return hmacHex(secret, ts+"\n"+nonce+"\n"+method+"\n"+path+"\n"+sha256Hex(body))
}

// SignCallback 网关→商户回调签名（docs/protocol.md §1.2）。
func SignCallback(secret, ts, nonce string, body []byte) string {
	return hmacHex(secret, ts+"\n"+nonce+"\n"+sha256Hex(body))
}

func randNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
