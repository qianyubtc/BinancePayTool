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
	AccountID       string `json:"account_id"`
	MerchantOrderID string `json:"merchant_order_id"`
	Status          string `json:"status"`
	Currency        string `json:"currency"`
	BaseAmount      string `json:"base_amount"`
	PayAmount       string `json:"pay_amount"`
	ActualAmount    string `json:"actual_amount"`
	NoteCode        string `json:"note_code"`
	PayURL          string `json:"pay_url"`
	ReceiveUID      string `json:"receive_uid"`
	ReceiveLink     string `json:"receive_link"` // 收款账号的币安收款链接（可自建收银页/二维码/唤起 App）
	ReceiveEmail    string `json:"receive_email"`
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
	AccountID       string `json:"account_id"`
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
	AccountID       string `json:"account_id,omitempty"` // 多账号模式：收款账号 ID，缺省为网关默认账号
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

// Account 网关里的一个收款账号（多账号模式，docs/protocol.md §2.6）。
type Account struct {
	AccountID    string `json:"account_id"`
	Label        string `json:"label"`
	APIKeyMasked string `json:"api_key_masked"`
	UID          string `json:"uid"`
	ReceiveLink  string `json:"receive_link"`
	ReceiveEmail string `json:"receive_email"`
	Status       string `json:"status"` // active | disabled
	LastOK       int64  `json:"last_ok"`
	LastErr      string `json:"last_err"`
	CreatedAt    int64  `json:"created_at"`
}

type CreateAccountReq struct {
	Label        string `json:"label,omitempty"`
	APIKey       string `json:"api_key"`    // 该收款账户的币安只读 API Key
	APISecret    string `json:"api_secret"` // 对应 Secret
	UID          string `json:"uid"`        // 该账户的币安 UID
	ReceiveLink  string `json:"receive_link,omitempty"`
	ReceiveEmail string `json:"receive_email,omitempty"`
}

// CreateAccount 添加（或按 api_key 更新）一个收款账号；网关会先用该 Key 试拉流水校验。
func (c *Client) CreateAccount(req CreateAccountReq) (*Account, error) {
	return c.doAccount("POST", "/api/v1/accounts", req)
}

func (c *Client) GetAccount(id string) (*Account, error) {
	return c.doAccount("GET", "/api/v1/accounts/"+id, nil)
}

// VerifyAccount 立即用该账号的 Key 试拉一次流水，失败返回 ERR_BINANCE。
func (c *Client) VerifyAccount(id string) (*Account, error) {
	return c.doAccount("POST", "/api/v1/accounts/"+id+"/verify", struct{}{})
}

func (c *Client) DisableAccount(id string) (*Account, error) {
	return c.doAccount("POST", "/api/v1/accounts/"+id+"/disable", struct{}{})
}

func (c *Client) EnableAccount(id string) (*Account, error) {
	return c.doAccount("POST", "/api/v1/accounts/"+id+"/enable", struct{}{})
}

func (c *Client) doAccount(method, path string, payload any) (*Account, error) {
	raw, err := c.call(method, path, payload)
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (c *Client) do(method, path string, payload any) (*Order, error) {
	raw, err := c.call(method, path, payload)
	if err != nil {
		return nil, err
	}
	var o Order
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func (c *Client) call(method, path string, payload any) (json.RawMessage, error) {
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
	return env.Data, nil
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
