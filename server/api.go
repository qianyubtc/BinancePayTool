package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	cfg      *Config
	st       *Store
	bc       *binanceClient
	matcher  *Matcher
	notifier *Notifier
	mux      *http.ServeMux

	nonceMu sync.Mutex
	nonces  map[string]int64 // nonce → 过期时间 ms

	rlMu sync.Mutex
	rl   map[string][]int64 // 限流：key → 时间戳窗口
}

func newApp(cfg *Config) (*App, error) {
	st, err := openStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	bc := newBinanceClient(cfg)
	a := &App{
		cfg: cfg, st: st, bc: bc,
		matcher:  newMatcher(cfg, st, bc),
		notifier: newNotifier(cfg, st),
		nonces:   map[string]int64{},
		rl:       map[string][]int64{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/orders", a.auth(a.handleCreate))
	mux.HandleFunc("GET /api/v1/orders/{id}", a.auth(a.handleGet))
	mux.HandleFunc("GET /api/v1/orders/by-merchant/{mid}", a.auth(a.handleGetByMerchant))
	mux.HandleFunc("POST /api/v1/orders/{id}/close", a.auth(a.handleClose))
	mux.HandleFunc("GET /pay/{token}", a.handleCashier)
	mux.HandleFunc("GET /pay/{token}/status", a.handleStatus)
	mux.HandleFunc("POST /pay/{token}/claim", a.handleClaim)
	mux.HandleFunc("GET /pay/{token}/qr", a.handleQR)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": version})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
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

func (a *App) orderJSON(o *Order) map[string]any {
	actual := ""
	if o.ActualAmount > 0 {
		actual = fmtAmount(o.ActualAmount)
	}
	return map[string]any{
		"order_id":          o.ID,
		"merchant_order_id": o.MerchantOrderID,
		"status":            o.Status,
		"currency":          o.Currency,
		"base_amount":       fmtAmount(o.BaseAmount),
		"pay_amount":        fmtAmount(o.PayAmount),
		"actual_amount":     actual,
		"note_code":         o.NoteCode,
		"pay_url":           a.cfg.BaseURL + "/pay/" + o.Token,
		"receive_uid":       a.cfg.BinanceUID,
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
	o, err := a.st.CreateOrder(a.cfg, req.MerchantOrderID, cur, base, ttl, req.CallbackURL, req.ReturnURL)
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
	log.Printf("[info] 新订单 %s %s %s 应付 %s", o.ID, o.Currency, fmtAmount(o.BaseAmount), fmtAmount(o.PayAmount))
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

func clientIP(r *http.Request) string {
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

var _ = time.Now
