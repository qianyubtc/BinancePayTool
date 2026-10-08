package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed templates/cashier.html
var cashierHTML string

var cashierTpl = template.Must(template.New("cashier").Parse(cashierHTML))

var reBinanceOrderID = regexp.MustCompile(`^[0-9]{6,20}$`)
var reMobileUA = regexp.MustCompile(`(?i)android|iphone|ipad|ipod|mobile|harmonyos`)

func isMobile(r *http.Request) bool { return reMobileUA.MatchString(r.UserAgent()) }

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
	acc, err := a.accountFor(o.AccountID)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	hasQR := acc.ReceiveLink != "" || (o.AccountID == defaultAccountID && (a.cfg.QRImage != "" || a.qrPNG != nil))
	// mode 仅用于体验站引导：note/claim 模式下引导付基础金额，让另外两种匹配方式有机会命中
	mode := demoMode(r.URL.Query().Get("mode"))
	showAmount := fmtAmount(o.PayAmount)
	amountHead, amountTailStr := amountTail(o.PayAmount, o.BaseAmount)
	if mode != "amount" {
		showAmount = fmtAmount(o.BaseAmount)
		amountHead, amountTailStr = showAmount, ""
	}
	noteLabel, noteHint := "转账备注（可选）", template.HTML("备注填 <b>"+o.NoteCode+"</b>（可选，能加速确认）")
	switch mode {
	case "note":
		noteLabel, noteHint = "转账备注（必填）", template.HTML("备注<b>必须</b>填 <b>"+o.NoteCode+"</b>")
	case "claim":
		noteLabel, noteHint = "转账备注（本次不填）", template.HTML("<b>不要填备注</b>，付完后在下方回填订单编号")
	}
	backURL := ""
	if a.cfg.DemoEnabled && strings.HasPrefix(o.MerchantOrderID, "DEMO-") {
		backURL = "/demo"
	}
	// 先渲染到缓冲区：模板出错时返回 500，而不是给付款方一张截断的半页
	var buf bytes.Buffer
	if err := cashierTpl.Execute(&buf, map[string]any{
		"BackURL":    backURL,
		"Mode":       mode,
		"ShowAmount": showAmount,
		"AmountHead": amountHead,
		"AmountTail": amountTailStr,
		"NoteLabel":  noteLabel,
		"NoteHint":   noteHint,
		"PayAmount":  fmtAmount(o.PayAmount),
		"Currency":   o.Currency,
		"UID":        acc.UID,
		"Email":      acc.ReceiveEmail,
		"HasQR":      hasQR,
		"AppLink":    acc.ReceiveLink,
		"IsMobile":   isMobile(r),
		"NoteCode":   o.NoteCode,
		"ExpiresAt":  o.ExpiresAt,
		"Now":        nowMs(), // 倒计时按服务端时间校正，付款方手机时间不准也不会提前显示超时
		"Token":      o.Token,
		"Status":     o.Status,
	}); err != nil {
		log.Printf("[error] 渲染收银页失败 order=%s: %v", o.ID, err)
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// amountTail 把唯一金额拆成「主体」和「识别尾数」，收银页高亮尾数，提醒付款方不要抹零：
// 尾数从基础金额的精度之后开始（5 → 5.0037 拆成 "5" + ".0037"，0.5 → 0.5037 拆成 "0.5" + "037"）；
// 基础金额已用满小数位时，从第一个与基础金额不同的数字开始。拆不出来时返回整串、尾数为空。
func amountTail(pay, base int64) (head, tail string) {
	s := fmtAmount(pay)
	if pay == base || pay <= 0 || base <= 0 {
		return s, ""
	}
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s, ""
	}
	cut := dot
	if bd := decimalsOf(base); bd > 0 {
		cut = dot + 1 + bd
	}
	if cut >= len(s) {
		// 补齐到 8 位小数逐位比较，找第一个不同的数字
		full := func(v int64) string { return fmt.Sprintf("%d.%08d", v/scale, v%scale) }
		p, b := full(pay), full(base)
		cut = -1
		for i := 0; i < len(p) && i < len(b); i++ {
			if p[i] != b[i] {
				cut = i
				break
			}
		}
		if cut >= len(s) {
			return s, ""
		}
	}
	if cut <= 0 {
		return s, ""
	}
	return s[:cut], s[cut:]
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
	actual := ""
	if o.ActualAmount > 0 {
		actual = fmtAmount(o.ActualAmount)
	}
	writeJSON(w, 200, map[string]any{
		"status":        o.Status,
		"paid_at":       o.PaidAt,
		"actual_amount": actual,
		"redirect":      a.redirectURL(o),
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
	if !a.allow("claim:"+tok, 5, 60) || !a.allow("claimip:"+a.clientIP(r), 20, 60) {
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

// handleQR 返回订单所属账号的收款二维码：默认账号优先 QR_IMAGE 文件，否则由收款链接服务端生成。
func (a *App) handleQR(w http.ResponseWriter, r *http.Request) {
	o, err := a.st.OrderByToken(r.PathValue("token"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var png []byte
	if o.AccountID == defaultAccountID {
		if a.cfg.QRImage != "" {
			http.ServeFile(w, r, a.cfg.QRImage)
			return
		}
		png = a.qrPNG
	} else if acc, err := a.accountFor(o.AccountID); err == nil && acc.ReceiveLink != "" {
		png = a.qrFor(acc.ReceiveLink)
	}
	if png == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(png)
}

// qrFor 按收款链接生成二维码并缓存。
func (a *App) qrFor(link string) []byte {
	a.qrMu.Lock()
	defer a.qrMu.Unlock()
	if png, ok := a.qrCache[link]; ok {
		return png
	}
	png, err := qrcode.Encode(link, qrcode.Medium, 360)
	if err != nil {
		return nil
	}
	if len(a.qrCache) > 500 {
		a.qrCache = map[string][]byte{}
	}
	a.qrCache[link] = png
	return png
}
