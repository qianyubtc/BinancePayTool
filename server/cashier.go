package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

//go:embed templates/cashier.html
var cashierHTML string

var cashierTpl = template.Must(template.New("cashier").Parse(cashierHTML))

var reBinanceOrderID = regexp.MustCompile(`^[0-9]{6,20}$`)

func (a *App) handleCashier(w http.ResponseWriter, r *http.Request) {
	o, err := a.st.OrderByToken(r.PathValue("token"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cashierTpl.Execute(w, map[string]any{
		"PayAmount": fmtAmount(o.PayAmount),
		"Currency":  o.Currency,
		"UID":       a.cfg.BinanceUID,
		"Email":     a.cfg.ReceiveEmail,
		"HasQR":     a.cfg.QRImage != "",
		"NoteCode":  o.NoteCode,
		"ExpiresAt": o.ExpiresAt,
		"Token":     o.Token,
	})
}

func (a *App) redirectURL(o *Order) string {
	if o.ReturnURL == "" || o.Status != "paid" {
		return ""
	}
	sep := "?"
	if strings.Contains(o.ReturnURL, "?") {
		sep = "&"
	}
	return o.ReturnURL + sep + "order_id=" + url.QueryEscape(o.ID) +
		"&merchant_order_id=" + url.QueryEscape(o.MerchantOrderID) + "&status=paid"
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	o, err := a.st.OrderByToken(r.PathValue("token"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{
		"status":   o.Status,
		"paid_at":  o.PaidAt,
		"redirect": a.redirectURL(o),
	})
}

func (a *App) handleClaim(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	o, err := a.st.OrderByToken(tok)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, 500, "ERR_INTERNAL", "内部错误")
		return
	}
	if !a.allow("claim:"+tok, 5, 60) || !a.allow("claimip:"+clientIP(r), 20, 60) {
		fail(w, 429, "ERR_RATE_LIMIT", "操作过于频繁")
		return
	}
	var req struct {
		BinanceOrderID string `json:"binance_order_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, "ERR_PARAM", "JSON 解析失败")
		return
	}
	req.BinanceOrderID = strings.TrimSpace(req.BinanceOrderID)
	if !reBinanceOrderID.MatchString(req.BinanceOrderID) {
		fail(w, 400, "ERR_PARAM", "订单编号格式错误")
		return
	}
	res, err := a.matcher.Claim(o, req.BinanceOrderID)
	if err != nil {
		fail(w, 502, "ERR_INTERNAL", "查询币安流水失败，请稍后再试")
		return
	}
	ok(w, map[string]any{"code": res.Code, "status": res.Status})
}

func (a *App) handleQR(w http.ResponseWriter, r *http.Request) {
	if a.cfg.QRImage == "" {
		http.NotFound(w, r)
		return
	}
	if _, err := a.st.OrderByToken(r.PathValue("token")); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, a.cfg.QRImage)
}
