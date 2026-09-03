package main

import (
	_ "embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
)

//go:embed templates/demo.html
var demoHTML string

var demoTpl = template.Must(template.New("demo").Parse(demoHTML))

const demoRepoURL = "https://github.com/qianyubtc/BinancePayTool"

func (a *App) demoCurrencies() []string {
	var out []string
	for c := range a.cfg.Currencies {
		out = append(out, c)
	}
	sort.Strings(out)
	// USDT 最常用，放到首位作为默认选项
	for i, c := range out {
		if c == "USDT" && i > 0 {
			out = append([]string{"USDT"}, append(out[:i:i], out[i+1:]...)...)
			break
		}
	}
	return out
}

func (a *App) renderDemo(w http.ResponseWriter, data map[string]any) {
	base := map[string]any{
		"Amounts":    a.cfg.DemoAmounts,
		"Currencies": a.demoCurrencies(),
		"HasApp":     a.cfg.ReceiveLink != "",
		"HasQR":      a.cfg.QRImage != "" || a.qrPNG != nil,
		"Repo":       demoRepoURL,
		"Version":    version,
	}
	for k, v := range data {
		base[k] = v
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	demoTpl.Execute(w, base)
}

func (a *App) handleDemo(w http.ResponseWriter, r *http.Request) {
	a.renderDemo(w, nil)
}

// handleDemoOrder 体验页下单：金额只允许 DEMO_AMOUNTS 中的档位，按 IP 限流。
func (a *App) handleDemoOrder(w http.ResponseWriter, r *http.Request) {
	if !a.allow("demo:"+a.clientIP(r), 5, 60) {
		a.renderDemo(w, map[string]any{"Error": "操作过于频繁，请一分钟后再试"})
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderDemo(w, map[string]any{"Error": "参数错误"})
		return
	}
	amount := strings.TrimSpace(r.FormValue("amount"))
	cur := strings.ToUpper(strings.TrimSpace(r.FormValue("currency")))
	allowed := false
	for _, x := range a.cfg.DemoAmounts {
		if x == amount {
			allowed = true
		}
	}
	if !allowed || !a.cfg.Currencies[cur] {
		a.renderDemo(w, map[string]any{"Error": "金额或币种不在体验范围内"})
		return
	}
	mode := demoMode(r.FormValue("mode"))
	base, _ := parseAmount(amount)
	o, err := a.st.CreateOrder(a.cfg, "DEMO-"+randString(idAlphabet, 12), cur, base, a.cfg.OrderTTL, "", a.cfg.BaseURL+"/demo/done")
	if err != nil {
		log.Printf("[error] 体验下单失败: %v", err)
		a.renderDemo(w, map[string]any{"Error": "创建订单失败，请稍后再试"})
		return
	}
	log.Printf("[info] 体验订单 %s %s %s 应付 %s 体验方式=%s", o.ID, o.Currency, amount, fmtAmount(o.PayAmount), mode)
	http.Redirect(w, r, "/pay/"+o.Token+"?mode="+mode, http.StatusFound)
}

// demoMode 规范化体验的到账确认方式：amount（唯一金额，默认）/ note（备注码）/ claim（回填订单编号）。
// 三种匹配在网关里始终同时生效，mode 只改变收银页的引导文案。
func demoMode(v string) string {
	switch v {
	case "note", "claim":
		return v
	}
	return "amount"
}

var matchedByLabel = map[string]string{
	"amount": "唯一金额自动匹配",
	"note":   "备注码匹配",
	"claim":  "回填订单编号核验",
}

func (a *App) handleDemoDone(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("order_id")
	o, err := a.st.OrderByID(id)
	if errors.Is(err, ErrNotFound) || err != nil {
		a.renderDemo(w, map[string]any{"Error": "订单不存在"})
		return
	}
	label := matchedByLabel[o.MatchedBy]
	if label == "" {
		label = o.MatchedBy
	}
	a.renderDemo(w, map[string]any{
		"Done":       true,
		"DoneStatus": o.Status,
		"DoneAmount": fmtAmount(o.ActualAmount),
		"DoneCur":    o.Currency,
		"DoneBy":     label,
	})
}
