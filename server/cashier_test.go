package main

import (
	"bytes"
	"html/template"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAmountTail(t *testing.T) {
	amt := func(s string) int64 {
		v, err := parseAmount(s)
		if err != nil {
			t.Fatalf("parseAmount(%q): %v", s, err)
		}
		return v
	}
	cases := []struct{ pay, base, head, tail string }{
		{"5.0037", "5", "5", ".0037"},      // 整数基础金额：小数部分全是尾数
		{"12.0099", "12", "12", ".0099"},   // 两位档上限
		{"0.5037", "0.5", "0.5", "037"},    // 基础金额带小数：从基础精度之后开始
		{"4.9963", "5", "4", ".9963"},      // SUFFIX_MODE=sub
		{"5.1271", "5.1234", "5.12", "71"}, // 基础金额已用满小数位：从第一个不同的数字开始
		{"5.13", "5.1234", "5.1", "3"},     // 尾零被去掉的情况
		{"9.9937", "9.99", "9.99", "37"},   //
		{"5", "5", "5", ""},                // 没有尾数（note/claim 模式展示基础金额）
		{"10.0001", "10", "10", ".0001"},   //
		{"1234.5678", "1234.5", "1234.5", "678"},
	}
	for _, c := range cases {
		h, tl := amountTail(amt(c.pay), amt(c.base))
		if h != c.head || tl != c.tail {
			t.Errorf("amountTail(%s, %s) = %q + %q，期望 %q + %q", c.pay, c.base, h, tl, c.head, c.tail)
		}
		if h+tl != c.pay {
			t.Errorf("amountTail(%s, %s) 拼回去应等于原金额，得到 %q", c.pay, c.base, h+tl)
		}
	}
}

// 收银页所有分支组合都必须完整渲染（曾因嵌套 {{end}} 放错把整块内容吞掉），且 JS 依赖的元素与函数都在。
func TestCashierTemplateStates(t *testing.T) {
	hooks := []string{`id="successCard"`, `id="succAmt"`, `id="redirectMsg"`, `id="redirectBtn"`, `id="payAmount"`,
		`id="cd"`, `id="st"`, `id="stTxt"`, `id="claimInput"`, `id="claimMsg"`, `id="p-manual"`, `id="claimCard"`,
		"function showTab(", "function cp(", "function tick(", "function setTxt(", "function onPaid(", "function poll(", "function claim(",
		`"/pay/"+token+"/status"`, `"/pay/"+token+"/claim"`, "</html>"}
	for _, status := range []string{"pending", "paid", "expired", "closed", "underpaid"} {
		for _, mode := range []string{"amount", "note", "claim"} {
			for mask := 0; mask < 16; mask++ {
				hasQR, link, email, mobile := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0
				data := map[string]any{
					"BackURL": "", "Mode": mode, "ShowAmount": "5.0037", "AmountHead": "5", "AmountTail": ".0037",
					"NoteLabel": "转账备注（可选）", "NoteHint": template.HTML("备注填 <b>AB12CD</b>（可选，能加速确认）"),
					"PayAmount": "5.0037", "Currency": "USDT", "UID": "90000001", "Email": "", "HasQR": hasQR, "AppLink": "",
					"IsMobile": mobile, "NoteCode": "AB12CD", "ExpiresAt": int64(1700000000000), "Now": int64(1699999000000),
					"Token": "tok123", "Status": status,
				}
				if link {
					data["AppLink"] = "https://app.binance.com/uni-qr/TEST"
				}
				if email {
					data["Email"] = "pay@example.com"
				}
				var buf bytes.Buffer
				if err := cashierTpl.Execute(&buf, data); err != nil {
					t.Fatalf("status=%s mode=%s mask=%d 渲染失败: %v", status, mode, mask, err)
				}
				h := buf.String()
				for _, want := range hooks {
					if !strings.Contains(h, want) {
						t.Fatalf("status=%s mode=%s mask=%d 缺少 %q", status, mode, mask, want)
					}
				}
				check := func(cond bool, s string) {
					if strings.Contains(h, s) != cond {
						t.Fatalf("status=%s mode=%s mask=%d：%q 出现=%v，期望 %v", status, mode, mask, s, !cond, cond)
					}
				}
				check(hasQR, `id="p-scan"`)
				check(hasQR, `src="/pay/tok123/qr"`)
				check(link, `id="p-app"`)
				check(link, `href="https://app.binance.com/uni-qr/TEST"`)
				check(email, "pay@example.com")
				check(!hasQR && !link, `class="tabs single"`)
				body := ""
				if mobile {
					body = "is-m"
				}
				switch status {
				case "paid":
					body += " is-paid"
				case "expired", "closed", "underpaid":
					body += " is-" + status + " is-end"
				}
				check(true, `<body class="`+body+`">`)
				check(mode == "claim", "<details open>")
				check(mode == "amount", "一分不差")
				// 对外收银页保持中性：非体验订单不出现体验 / 站点品牌字样
				if mode == "amount" {
					for _, bad := range []string{"体验", "demo", "Demo", "Bnsbot", "千羽"} {
						check(false, bad)
					}
				}
			}
		}
	}
}

// 收银页按订单当前状态直接渲染初始界面（已付款不再闪一下付款表单），并高亮唯一金额的识别尾数。
func TestCashierInitialState(t *testing.T) {
	e := newEnv(t)
	get := func(token string) string {
		resp, err := http.Get(e.srv.URL + "/pay/" + token)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("收银页 HTTP %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return string(b)
	}
	d := e.create("M-init-1", "USDT", "5", "")
	token := d["pay_url"].(string)[len("http://gw.test/pay/"):]
	pay := d["pay_amount"].(string)
	h := get(token)
	if !strings.Contains(h, `id="payAmount">5<span class="tail">`+strings.TrimPrefix(pay, "5")+`</span><small>USDT</small>`) {
		t.Fatalf("唯一金额 %s 应拆出识别尾数高亮", pay)
	}
	if !strings.Contains(h, `<body class="">`) || !strings.Contains(h, ">等待支付<") {
		t.Fatal("待支付订单初始状态不对")
	}
	if _, err := e.app.st.CloseOrder(d["order_id"].(string), e.app.cfg.SuffixCooldown); err != nil {
		t.Fatal(err)
	}
	h = get(token)
	if !strings.Contains(h, `<body class=" is-closed is-end">`) || !strings.Contains(h, `class="bad">订单已关闭<`) {
		t.Fatal("已关闭订单应直接渲染关闭状态")
	}
}
