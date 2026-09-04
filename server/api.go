package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
)

type App struct {
	cfg      *Config
	st       *Store
	matcher  *Matcher
	notifier *Notifier
	mux      *http.ServeMux

	nonceMu sync.Mutex
	nonces  map[string]int64 // nonce → 过期时间 ms

	rlMu sync.Mutex
	rl   map[string][]int64 // 限流：key → 时间戳窗口

	qrPNG []byte // 由 RECEIVE_LINK 生成的收款二维码（启动时生成一次）

	qrMu    sync.Mutex
	qrCache map[string][]byte // 动态账号收款链接 → 二维码
}

func newApp(cfg *Config) (*App, error) {
	st, err := openStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	st.key = deriveKey(cfg.APIAuthKey)
	a := &App{
		cfg: cfg, st: st,
		matcher:  newMatcher(cfg, st),
		notifier: newNotifier(cfg, st),
		nonces:   map[string]int64{},
		rl:       map[string][]int64{},
		qrCache:  map[string][]byte{},
	}
	if cfg.ReceiveLink != "" && cfg.QRImage == "" {
		png, err := qrcode.Encode(cfg.ReceiveLink, qrcode.Medium, 360)
		if err != nil {
			return nil, fmt.Errorf("生成收款二维码失败: %w", err)
		}
		a.qrPNG = png
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/orders", a.auth(a.handleCreate))
	mux.HandleFunc("GET /api/v1/orders/{id}", a.auth(a.handleGet))
	mux.HandleFunc("GET /api/v1/orders/by-merchant/{mid}", a.auth(a.handleGetByMerchant))
	mux.HandleFunc("POST /api/v1/orders/{id}/close", a.auth(a.handleClose))
	mux.HandleFunc("POST /api/v1/accounts", a.auth(a.handleCreateAccount))
	mux.HandleFunc("GET /api/v1/accounts/{id}", a.auth(a.handleGetAccount))
	mux.HandleFunc("POST /api/v1/accounts/{id}/verify", a.auth(a.handleVerifyAccount))
	mux.HandleFunc("POST /api/v1/accounts/{id}/disable", a.auth(a.handleAccountStatus("disabled")))
	mux.HandleFunc("POST /api/v1/accounts/{id}/enable", a.auth(a.handleAccountStatus("active")))
	mux.HandleFunc("GET /pay/{token}", a.handleCashier)
	mux.HandleFunc("GET /pay/{token}/status", a.handleStatus)
	mux.HandleFunc("POST /pay/{token}/claim", a.handleClaim)
	mux.HandleFunc("GET /pay/{token}/qr", a.handleQR)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": version})
	})
	if cfg.DemoEnabled {
		mux.HandleFunc("GET /demo", a.handleDemo)
		mux.HandleFunc("POST /demo/order", a.handleDemoOrder)
		mux.HandleFunc("GET /demo/done", a.handleDemoDone)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if cfg.DemoEnabled {
			http.Redirect(w, r, "/demo", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("BinancePayTool " + version + " running\n"))
	})
	a.mux = mux
	return a, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, 200, map[string]any{"code": "OK", "data": data})
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"code": code, "message": msg})
}

// auth 校验商户请求签名（docs/protocol.md §1.1）。
func (a *App) auth(next func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			fail(w, 400, "ERR_PARAM", "读取请求体失败")
			return
		}
		ts := r.Header.Get("X-BPG-Timestamp")
		nonce := r.Header.Get("X-BPG-Nonce")
		sig := r.Header.Get("X-BPG-Signature")
		if ts == "" || len(nonce) < 16 || sig == "" {
			fail(w, 401, "ERR_AUTH", "缺少签名头")
			return
		}
		tsv, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || tsv < nowMs()-300000 || tsv > nowMs()+300000 {
			fail(w, 401, "ERR_AUTH", "时间戳无效或偏差过大")
			return
		}
		expect := signRequest(a.cfg.APIAuthKey, ts, nonce, r.Method, r.URL.Path, body)
		if !sigEqual(expect, sig) {
			fail(w, 401, "ERR_AUTH", "签名错误")
			return
		}
		a.nonceMu.Lock()
		now := nowMs()
		if len(a.nonces) > 10000 {
			for k, exp := range a.nonces {
				if exp < now {
					delete(a.nonces, k)
				}
			}
		}
		if exp, seen := a.nonces[nonce]; seen && exp > now {
			a.nonceMu.Unlock()
			fail(w, 401, "ERR_AUTH", "nonce 重复")
			return
		}
		a.nonces[nonce] = now + 300000
		a.nonceMu.Unlock()
		next(w, r, body)
	}
}

// allow 简单滑动窗口限流。
func (a *App) allow(key string, max int, windowSec int64) bool {
	a.rlMu.Lock()
	defer a.rlMu.Unlock()
	now := nowMs()
	lo := now - windowSec*1000
	win := a.rl[key]
	var kept []int64
	for _, t := range win {
		if t >= lo {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		a.rl[key] = kept
		return false
	}
	a.rl[key] = append(kept, now)
	return true
}

// accountFor 取订单所属收款账号：default 来自 config.env，其余来自账号表。
func (a *App) accountFor(id string) (*Account, error) {
	if id == "" || id == defaultAccountID {
		return defaultAccount(a.cfg), nil
	}
	return a.st.GetAccount(id)
}

func (a *App) orderJSON(o *Order) map[string]any {
	actual := ""
	if o.ActualAmount > 0 {
		actual = fmtAmount(o.ActualAmount)
	}
	uid, link, email := a.cfg.BinanceUID, a.cfg.ReceiveLink, a.cfg.ReceiveEmail
	if acc, err := a.accountFor(o.AccountID); err == nil {
		uid, link, email = acc.UID, acc.ReceiveLink, acc.ReceiveEmail
	}
	return map[string]any{
		"order_id":          o.ID,
		"account_id":        o.AccountID,
		"receive_link":      link,
		"receive_email":     email,
		"merchant_order_id": o.MerchantOrderID,
		"status":            o.Status,
		"currency":          o.Currency,
		"base_amount":       fmtAmount(o.BaseAmount),
		"pay_amount":        fmtAmount(o.PayAmount),
		"actual_amount":     actual,
		"note_code":         o.NoteCode,
		"pay_url":           a.cfg.BaseURL + "/pay/" + o.Token,
		"receive_uid":       uid,
		"matched_by":        o.MatchedBy,
		"binance_order_id":  o.BinanceOrderID,
		"binance_txn_id":    o.BinanceTxnID,
		"payer_id":          o.PayerID,
		"overpaid":          o.Overpaid,
		"created_at":        o.CreatedAt,
		"expires_at":        o.ExpiresAt,
		"paid_at":           o.PaidAt,
	}
}

type createReq struct {
	AccountID       string `json:"account_id"`
	MerchantOrderID string `json:"merchant_order_id"`
	Currency        string `json:"currency"`
	Amount          string `json:"amount"`
	CallbackURL     string `json:"callback_url"`
	ReturnURL       string `json:"return_url"`
	Timeout         int64  `json:"timeout"`
}

func (a *App) handleCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	var req createReq
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, 400, "ERR_PARAM", "JSON 解析失败")
		return
	}
	req.MerchantOrderID = strings.TrimSpace(req.MerchantOrderID)
	if req.MerchantOrderID == "" || len(req.MerchantOrderID) > 64 {
		fail(w, 400, "ERR_PARAM", "merchant_order_id 必填且 ≤64 字符")
		return
	}
	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	if !a.cfg.Currencies[cur] {
		fail(w, 400, "ERR_PARAM", "currency 不在允许列表内")
		return
	}
	base, err := parseAmount(req.Amount)
	if err != nil || base <= 0 {
		fail(w, 400, "ERR_PARAM", "amount 非法")
		return
	}
	if decimalsOf(base) > a.cfg.AmountDecimals {
		fail(w, 400, "ERR_PARAM", "amount 小数位超过网关 AMOUNT_DECIMALS")
		return
	}
	ttl := req.Timeout
	if ttl == 0 {
		ttl = a.cfg.OrderTTL
	}
	if ttl < 120 || ttl > 86400 {
		fail(w, 400, "ERR_PARAM", "timeout 须在 120~86400 秒之间")
		return
	}
	accID := strings.TrimSpace(req.AccountID)
	if accID == "" {
		accID = defaultAccountID
	}
	if accID != defaultAccountID {
		acc, err := a.st.GetAccount(accID)
		if errors.Is(err, ErrNotFound) || (err == nil && acc.Status != "active") {
			fail(w, 400, "ERR_PARAM", "account_id 不存在或已停用")
			return
		}
		if err != nil {
			log.Printf("[error] 读取账号 %s 失败: %v", accID, err)
			fail(w, 500, "ERR_INTERNAL", "内部错误")
			return
		}
	}
	o, err := a.st.CreateOrder(a.cfg, accID, req.MerchantOrderID, cur, base, ttl, req.CallbackURL, req.ReturnURL)
	if errors.Is(err, ErrDuplicate) {
		fail(w, 409, "ERR_DUPLICATE", "merchant_order_id 已存在")
		return
	}
	if errors.Is(err, ErrAlloc) {
		fail(w, 503, "ERR_ALLOC", "该金额档位唯一尾数已耗尽，请稍后或换金额")
		return
	}
	if err != nil {
		log.Printf("[error] 创建订单失败: %v", err)
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	log.Printf("[info] 新订单 %s account=%s %s %s 应付 %s", o.ID, o.AccountID, o.Currency, fmtAmount(o.BaseAmount), fmtAmount(o.PayAmount))
	ok(w, a.orderJSON(o))
}

func (a *App) handleGet(w http.ResponseWriter, r *http.Request, _ []byte) {
	o, err := a.st.OrderByID(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "ERR_NOT_FOUND", "订单不存在")
		return
	}
	if err != nil {
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	ok(w, a.orderJSON(o))
}

func (a *App) handleGetByMerchant(w http.ResponseWriter, r *http.Request, _ []byte) {
	o, err := a.st.OrderByMerchantID(r.PathValue("mid"))
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "ERR_NOT_FOUND", "订单不存在")
		return
	}
	if err != nil {
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	ok(w, a.orderJSON(o))
}

func (a *App) handleClose(w http.ResponseWriter, r *http.Request, _ []byte) {
	o, err := a.st.CloseOrder(r.PathValue("id"), a.cfg.SuffixCooldown)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "ERR_NOT_FOUND", "订单不存在")
		return
	}
	if errors.Is(err, ErrState) {
		fail(w, 409, "ERR_STATE", "仅待支付订单可关闭")
		return
	}
	if err != nil {
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	ok(w, a.orderJSON(o))
}

// clientIP 取访客 IP。直连时用 RemoteAddr；TRUST_PROXY 开启时依次信任
// CF-Connecting-IP（Cloudflare）、X-Real-IP、X-Forwarded-For 首个地址。
func (a *App) clientIP(r *http.Request) string {
	if a.cfg.TrustProxy {
		for _, h := range []string{"CF-Connecting-IP", "X-Real-IP"} {
			if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
				return v
			}
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

// ---------- 收款账号（多账号模式）----------

type accountReq struct {
	Label        string `json:"label"`
	APIKey       string `json:"api_key"`
	APISecret    string `json:"api_secret"`
	UID          string `json:"uid"`
	ReceiveLink  string `json:"receive_link"`
	ReceiveEmail string `json:"receive_email"`
}

var reUID = regexp.MustCompile(`^[0-9]{1,20}$`)

func (a *App) accountJSON(acc *Account) map[string]any {
	masked := acc.APIKey
	if len(masked) > 8 {
		masked = masked[:4] + "…" + masked[len(masked)-4:]
	}
	return map[string]any{
		"account_id":     acc.ID,
		"label":          acc.Label,
		"api_key_masked": masked,
		"uid":            acc.UID,
		"receive_link":   acc.ReceiveLink,
		"receive_email":  acc.ReceiveEmail,
		"status":         acc.Status,
		"last_ok":        acc.LastOK,
		"last_err":       acc.LastErr,
		"created_at":     acc.CreatedAt,
	}
}

// verifyBinanceKey 用只读接口试拉一条流水：Key/Secret/权限/IP 白名单/地区限制任一不对都会报错。
func (a *App) verifyBinanceKey(key, secret string) error {
	_, err := newBinanceClientFor(a.cfg.BinanceAPIBase, key, secret).PayTransactions(0, 0, 1)
	return err
}

func (a *App) handleCreateAccount(w http.ResponseWriter, r *http.Request, body []byte) {
	var req accountReq
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, 400, "ERR_PARAM", "JSON 解析失败")
		return
	}
	req.APIKey, req.APISecret = strings.TrimSpace(req.APIKey), strings.TrimSpace(req.APISecret)
	req.UID, req.Label = strings.TrimSpace(req.UID), strings.TrimSpace(req.Label)
	req.ReceiveLink, req.ReceiveEmail = strings.TrimSpace(req.ReceiveLink), strings.TrimSpace(req.ReceiveEmail)
	switch {
	case req.APIKey == "" || req.APISecret == "" || len(req.APIKey) > 128 || len(req.APISecret) > 128:
		fail(w, 400, "ERR_PARAM", "api_key / api_secret 必填")
		return
	case !reUID.MatchString(req.UID):
		fail(w, 400, "ERR_PARAM", "uid 须为数字")
		return
	case req.ReceiveLink != "" && !strings.HasPrefix(req.ReceiveLink, "https://"):
		fail(w, 400, "ERR_PARAM", "receive_link 须为 https:// 链接")
		return
	case len(req.Label) > 64 || len(req.ReceiveLink) > 300 || len(req.ReceiveEmail) > 100:
		fail(w, 400, "ERR_PARAM", "字段过长")
		return
	}
	if err := a.verifyBinanceKey(req.APIKey, req.APISecret); err != nil {
		fail(w, 400, "ERR_BINANCE", "币安校验失败: "+truncate(err.Error(), 200))
		return
	}
	acc := &Account{Label: req.Label, APIKey: req.APIKey, APISecret: req.APISecret, UID: req.UID, ReceiveLink: req.ReceiveLink, ReceiveEmail: req.ReceiveEmail}
	if err := a.st.UpsertAccount(acc); err != nil {
		log.Printf("[error] 保存账号失败: %v", err)
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	a.st.AccountPolled(acc.ID, true, "")
	acc.LastOK = nowMs()
	log.Printf("[info] 收款账号 %s 已添加/更新 uid=%s label=%s", acc.ID, acc.UID, acc.Label)
	ok(w, a.accountJSON(acc))
}

func (a *App) handleGetAccount(w http.ResponseWriter, r *http.Request, _ []byte) {
	acc, err := a.accountFor(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "ERR_NOT_FOUND", "账号不存在")
		return
	}
	if err != nil {
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	ok(w, a.accountJSON(acc))
}

func (a *App) handleVerifyAccount(w http.ResponseWriter, r *http.Request, _ []byte) {
	id := r.PathValue("id")
	acc, err := a.accountFor(id)
	if errors.Is(err, ErrNotFound) {
		fail(w, 404, "ERR_NOT_FOUND", "账号不存在")
		return
	}
	if err != nil {
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	if err := a.verifyBinanceKey(acc.APIKey, acc.APISecret); err != nil {
		msg := truncate(err.Error(), 200)
		if id != defaultAccountID {
			a.st.AccountPolled(id, false, msg)
		}
		fail(w, 400, "ERR_BINANCE", "币安校验失败: "+msg)
		return
	}
	if id != defaultAccountID {
		a.st.AccountPolled(id, true, "")
		acc.LastOK, acc.LastErr = nowMs(), ""
	}
	ok(w, a.accountJSON(acc))
}

func (a *App) handleAccountStatus(status string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		id := r.PathValue("id")
		if id == defaultAccountID {
			fail(w, 400, "ERR_PARAM", "默认账号由 config.env 管理")
			return
		}
		if err := a.st.SetAccountStatus(id, status); errors.Is(err, ErrNotFound) {
			fail(w, 404, "ERR_NOT_FOUND", "账号不存在")
			return
		} else if err != nil {
			fail(w, 500, "ERR_INTERNAL", "内部错误")
			return
		}
		acc, err := a.st.GetAccount(id)
		if err != nil {
			fail(w, 500, "ERR_INTERNAL", "内部错误")
			return
		}
		log.Printf("[info] 收款账号 %s → %s", id, status)
		ok(w, a.accountJSON(acc))
	}
}
