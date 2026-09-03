package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newDemoEnv(t *testing.T) *env {
	mock := newMockBinance()
	cfg := &Config{
		Listen: "127.0.0.1:0", BaseURL: "http://gw.test", APIAuthKey: testSecret,
		BinanceKey: "k", BinanceSecret: "s", BinanceUID: "90000001",
		BinanceAPIBase: mock.srv.URL,
		Currencies:     map[string]bool{"USDT": true},
		OrderTTL:       900, ExpiredGrace: 1800, SuffixCooldown: 86400, PollInterval: 1,
		AmountDecimals: 4, SuffixMode: "add",
		DBPath:      t.TempDir() + "/demo.db",
		ReceiveLink: "https://app.binance.com/qr/dplkTESTLINK",
		DemoEnabled: true, DemoAmounts: []string{"0.5", "1"},
	}
	app, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	srv := httptest.NewServer(app.mux)
	_, cancel := context.WithCancel(context.Background())
	e := &env{t: t, app: app, srv: srv, mock: mock}
	t.Cleanup(func() { cancel(); srv.Close(); mock.srv.Close(); app.st.Close() })
	return e
}

func TestDemoFlowAndThreeModes(t *testing.T) {
	e := newDemoEnv(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// 根路径跳转体验页；体验页可渲染
	resp, _ := noRedirect.Get(e.srv.URL + "/")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/demo" {
		t.Fatalf("根路径应跳转 /demo: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = http.Get(e.srv.URL + "/demo")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "体验") {
		t.Fatalf("体验页渲染失败: %d", resp.StatusCode)
	}

	// 体验下单 → 302 到收银页
	form := url.Values{"amount": {"0.5"}, "currency": {"USDT"}}
	resp, _ = noRedirect.Post(e.srv.URL+"/demo/order", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || !strings.HasPrefix(loc, "/pay/") {
		t.Fatalf("体验下单应跳转收银页: %d %s", resp.StatusCode, loc)
	}
	token := strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/pay/") // 跳转带 ?mode=，取 token 时去掉查询串

	// 服务端生成的二维码
	resp, _ = http.Get(e.srv.URL + "/pay/" + token + "/qr")
	png, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatalf("二维码应为 PNG: %d %d bytes", resp.StatusCode, len(png))
	}

	// 桌面 UA：有扫码与 App 两个标签；手机 UA：默认切到 App
	req, _ := http.NewRequest("GET", e.srv.URL+"/pay/"+token, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh)")
	resp, _ = http.DefaultClient.Do(req)
	html, _ := io.ReadAll(resp.Body)
	h := strings.ReplaceAll(string(html), " ", "") // html/template 在 JS 上下文输出布尔值会加空格
	// 页面内所有异步请求必须用带 token 的绝对路径：/pay/{token} 无尾斜杠，相对路径会解析到 /pay/xxx
	for _, want := range []string{"扫码支付", "打开币安App", "手动转账", "https://app.binance.com/qr/dplkTESTLINK", "isMobile=false",
		"/pay/" + token + "/qr", "/pay/\"+token+\"/status", "/pay/\"+token+\"/claim"} {
		if !strings.Contains(h, want) {
			t.Fatalf("桌面收银页缺少 %q", want)
		}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)")
	resp, _ = http.DefaultClient.Do(req)
	html, _ = io.ReadAll(resp.Body)
	if !strings.Contains(strings.ReplaceAll(string(html), " ", ""), "isMobile=true") {
		t.Fatal("手机 UA 应标记 isMobile=true")
	}

	// 非法金额被拒
	form = url.Values{"amount": {"999"}, "currency": {"USDT"}}
	resp, _ = noRedirect.Post(e.srv.URL+"/demo/order", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "不在体验范围内") {
		t.Fatalf("非法金额应被拒: %d", resp.StatusCode)
	}

	// 限流：同 IP 一分钟 5 次
	hit429 := false
	for i := 0; i < 6; i++ {
		form = url.Values{"amount": {"1"}, "currency": {"USDT"}}
		resp, _ = noRedirect.Post(e.srv.URL+"/demo/order", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		b, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(b), "操作过于频繁") {
			hit429 = true
		}
	}
	if !hit429 {
		t.Fatal("体验下单应被限流")
	}
}

func TestDemoDisabledByDefault(t *testing.T) {
	e := newEnv(t)
	resp, _ := http.Get(e.srv.URL + "/demo")
	if resp.StatusCode != 404 {
		t.Fatalf("未启用时 /demo 应 404，得到 %d", resp.StatusCode)
	}
}

func TestTrustProxyClientIP(t *testing.T) {
	e := newDemoEnv(t)
	e.app.cfg.TrustProxy = true
	req, _ := http.NewRequest("GET", e.srv.URL+"/demo", nil)
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 10.0.0.1")
	if ip := e.app.clientIP(req); ip != "203.0.113.9" {
		t.Fatalf("应优先 CF-Connecting-IP，得到 %s", ip)
	}
	req.Header.Del("CF-Connecting-IP")
	if ip := e.app.clientIP(req); ip != "198.51.100.1" {
		t.Fatalf("应取 X-Forwarded-For 首个地址，得到 %s", ip)
	}
	e.app.cfg.TrustProxy = false
	req.RemoteAddr = "127.0.0.1:5555"
	if ip := e.app.clientIP(req); ip != "127.0.0.1" {
		t.Fatalf("未开启 TRUST_PROXY 应忽略代理头，得到 %s", ip)
	}
}

// 体验站三种到账确认方式的引导：note/claim 模式显示基础金额并调整备注文案，claim 模式回填区默认展开
func TestDemoConfirmModes(t *testing.T) {
	e := newDemoEnv(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(mode string) string {
		form := url.Values{"amount": {"0.5"}, "currency": {"USDT"}, "mode": {mode}}
		resp, _ := noRedirect.Post(e.srv.URL+"/demo/order", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		loc := resp.Header.Get("Location")
		if !strings.Contains(loc, "?mode="+demoMode(mode)) {
			t.Fatalf("mode=%s 跳转应带 mode 参数: %s", mode, loc)
		}
		resp, _ = http.Get(e.srv.URL + loc)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}
	h := get("amount")
	if !strings.Contains(h, "一分不差") || strings.Contains(h, "0.5<small>") {
		t.Fatal("amount 模式应显示唯一金额与一分不差提示")
	}
	h = get("note")
	if !strings.Contains(h, "0.5<small>") || !strings.Contains(h, "备注<b>必须</b>填") || !strings.Contains(h, "转账备注（必填）") {
		t.Fatal("note 模式应显示基础金额与备注必填")
	}
	h = get("claim")
	if !strings.Contains(h, "0.5<small>") || !strings.Contains(h, "<details open>") || !strings.Contains(h, "不要填备注") {
		t.Fatal("claim 模式应显示基础金额、不填备注、回填区展开")
	}
	if get("bogus") == "" || demoMode("bogus") != "amount" {
		t.Fatal("非法 mode 应回落到 amount")
	}
}
