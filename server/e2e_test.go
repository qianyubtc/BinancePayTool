package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "test-secret-0123456789abcdef"

// ---- mock 币安 ----

// mock 币安按 API Key 区分账号：不同 Key 看到各自的流水；Key 为 bad 时返回 -2015。
type mockBinance struct {
	mu   sync.Mutex
	txns map[string][]payTxn
	srv  *httptest.Server
}

func newMockBinance() *mockBinance {
	m := &mockBinance{txns: map[string][]payTxn{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/sapi/v1/pay/transactions", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-MBX-APIKEY")
		if key == "bad" {
			writeJSON(w, 401, map[string]any{"code": -2015, "msg": "Invalid API-key, IP, or permissions for action."})
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		list := m.txns[key]
		if list == nil {
			list = []payTxn{}
		}
		writeJSON(w, 200, map[string]any{"code": "000000", "message": "success", "data": list, "success": true})
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockBinance) inject(t payTxn) { m.injectFor("k", t) }

func (m *mockBinance) injectFor(key string, t payTxn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.txns[key] = append(m.txns[key], t)
}

// ---- 商户回调接收器 ----

type cbRecv struct {
	body    []byte
	headers http.Header
}

func newCallbackServer(ch chan cbRecv) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ch <- cbRecv{body: body, headers: r.Header.Clone()}
		w.WriteHeader(200)
	}))
}

// ---- 测试环境 ----

type env struct {
	t    *testing.T
	app  *App
	srv  *httptest.Server
	mock *mockBinance
	cbCh chan cbRecv
	cbS  *httptest.Server
}

func newEnv(t *testing.T) *env {
	mock := newMockBinance()
	cfg := &Config{
		Listen: "127.0.0.1:0", BaseURL: "http://gw.test", APIAuthKey: testSecret,
		BinanceKey: "k", BinanceSecret: "s", BinanceUID: "90000001",
		BinanceAPIBase: mock.srv.URL,
		Currencies:     map[string]bool{"USDT": true, "USDC": true},
		OrderTTL:       900, ExpiredGrace: 1800, SuffixCooldown: 86400, PollInterval: 1,
		AmountDecimals: 4, SuffixMode: "add",
		DBPath: t.TempDir() + "/test.db", LogLevel: "info",
	}
	app, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	srv := httptest.NewServer(app.mux)
	ctx, cancel := context.WithCancel(context.Background())
	go app.matcher.run(ctx)
	go app.notifier.run(ctx)
	ch := make(chan cbRecv, 16)
	cbS := newCallbackServer(ch)
	e := &env{t: t, app: app, srv: srv, mock: mock, cbCh: ch, cbS: cbS}
	t.Cleanup(func() {
		cancel()
		srv.Close()
		cbS.Close()
		mock.srv.Close()
		app.st.Close()
	})
	return e
}

func (e *env) doSigned(method, path string, body []byte) (int, map[string]any) {
	ts := strconv.FormatInt(nowMs(), 10)
	nonce := newNonce()
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BPG-Timestamp", ts)
	req.Header.Set("X-BPG-Nonce", nonce)
	req.Header.Set("X-BPG-Signature", signRequest(testSecret, ts, nonce, method, path, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) create(mid, currency, amount, cb string) map[string]any {
	body, _ := json.Marshal(map[string]any{
		"merchant_order_id": mid, "currency": currency, "amount": amount, "callback_url": cb,
	})
	code, out := e.doSigned("POST", "/api/v1/orders", body)
	if code != 200 {
		e.t.Fatalf("创建订单失败 %d: %v", code, out)
	}
	return out["data"].(map[string]any)
}

func (e *env) waitStatus(orderID, want string, timeout time.Duration) map[string]any {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		code, out := e.doSigned("GET", "/api/v1/orders/"+orderID, nil)
		if code == 200 {
			d := out["data"].(map[string]any)
			if d["status"] == want {
				return d
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatalf("订单 %s 未在 %v 内达到状态 %s", orderID, timeout, want)
	return nil
}

func (e *env) waitCallback(timeout time.Duration) cbRecv {
	select {
	case r := <-e.cbCh:
		return r
	case <-time.After(timeout):
		e.t.Fatal("等待回调超时")
		return cbRecv{}
	}
}

var txnSeq int64 = 452000000000000000
var txnSeqMu sync.Mutex

func nextBinanceOrderID() string {
	txnSeqMu.Lock()
	defer txnSeqMu.Unlock()
	txnSeq++
	return strconv.FormatInt(txnSeq, 10)
}

func mkTxn(binanceOrderID, amount, currency, note string) payTxn {
	var t payTxn
	t.OrderID = binanceOrderID
	t.Amount = amount
	t.Currency = currency
	t.Note = note
	t.OrderType = "C2C"
	t.TransactionID = "P_TEST" + binanceOrderID[len(binanceOrderID)-6:]
	t.Time = nowMs()
	t.CounterpartyID = 1163000000
	t.PayerInfo.Name = "tester"
	return t
}

// ① 唯一金额自动匹配 + 回调验签
func TestAmountMatchFlow(t *testing.T) {
	e := newEnv(t)
	d := e.create("M-amount-1", "USDT", "10", e.cbS.URL)
	payAmount := d["pay_amount"].(string)
	if payAmount == "10" {
		t.Fatalf("pay_amount 应带唯一尾数，得到 %s", payAmount)
	}
	bid := nextBinanceOrderID()
	e.mock.inject(mkTxn(bid, payAmount, "USDT", ""))

	got := e.waitStatus(d["order_id"].(string), "paid", 10*time.Second)
	if got["matched_by"] != "amount" || got["binance_order_id"] != bid {
		t.Fatalf("匹配信息错误: %v", got)
	}
	cb := e.waitCallback(10 * time.Second)
	var payload map[string]any
	if err := json.Unmarshal(cb.body, &payload); err != nil {
		t.Fatalf("回调体解析: %v", err)
	}
	if payload["event"] != "paid" || payload["merchant_order_id"] != "M-amount-1" {
		t.Fatalf("回调内容错误: %v", payload)
	}
	ts := cb.headers.Get("X-BPG-Timestamp")
	nonce := cb.headers.Get("X-BPG-Nonce")
	sig := cb.headers.Get("X-BPG-Signature")
	if signCallback(testSecret, ts, nonce, cb.body) != sig {
		t.Fatal("回调签名校验失败")
	}
	// 已支付订单：状态接口返回实收金额，收银页含整页成功态
	token := d["pay_url"].(string)[len("http://gw.test/pay/"):]
	resp, err := http.Get(e.srv.URL + "/pay/" + token + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["status"] != "paid" || st["actual_amount"] != payAmount {
		t.Fatalf("状态接口应带实收金额: %v", st)
	}
	resp, _ = http.Get(e.srv.URL + "/pay/" + token)
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(html), `id="successCard"`) || !strings.Contains(string(html), "支付成功") {
		t.Fatal("收银页缺少成功态面板")
	}
}

// ② 备注码匹配（金额不唯一也能确认）
func TestNoteMatchFlow(t *testing.T) {
	e := newEnv(t)
	d := e.create("M-note-1", "USDT", "25.5", e.cbS.URL)
	code := d["note_code"].(string)
	bid := nextBinanceOrderID()
	e.mock.inject(mkTxn(bid, "25.5", "USDT", "订单 "+code+" 谢谢"))
	got := e.waitStatus(d["order_id"].(string), "paid", 10*time.Second)
	if got["matched_by"] != "note" || got["actual_amount"] != "25.5" {
		t.Fatalf("备注匹配信息错误: %v", got)
	}
}

// ②b 备注命中但实付不足 → underpaid
func TestUnderpaidViaNote(t *testing.T) {
	e := newEnv(t)
	d := e.create("M-under-1", "USDT", "30", e.cbS.URL)
	code := d["note_code"].(string)
	e.mock.inject(mkTxn(nextBinanceOrderID(), "29", "USDT", code))
	got := e.waitStatus(d["order_id"].(string), "underpaid", 10*time.Second)
	if got["actual_amount"] != "29" {
		t.Fatalf("underpaid 信息错误: %v", got)
	}
	cb := e.waitCallback(10 * time.Second)
	var payload map[string]any
	json.Unmarshal(cb.body, &payload)
	if payload["event"] != "underpaid" {
		t.Fatalf("回调事件应为 underpaid: %v", payload)
	}
}

// ③ 收银页回填币安订单编号
func TestClaimFlow(t *testing.T) {
	e := newEnv(t)
	d := e.create("M-claim-1", "USDT", "40", e.cbS.URL)
	bid := nextBinanceOrderID()
	e.mock.inject(mkTxn(bid, "40", "USDT", "")) // 金额=基础值且无备注 → 自动路径不命中
	payURL := d["pay_url"].(string)
	token := payURL[len("http://gw.test/pay/"):]
	body, _ := json.Marshal(map[string]string{"binance_order_id": bid})
	resp, err := http.Post(e.srv.URL+"/pay/"+token+"/claim", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	data, okCast := out["data"].(map[string]any)
	if !okCast {
		t.Fatalf("claim 响应异常: %v", out)
	}
	if data["code"] != "OK" || data["status"] != "paid" {
		t.Fatalf("claim 结果错误: %v", out)
	}
	got := e.waitStatus(d["order_id"].(string), "paid", 5*time.Second)
	if got["matched_by"] != "claim" {
		t.Fatalf("matched_by 应为 claim: %v", got)
	}
}

// 过期 → 宽限期内精确金额到账 → 复活为已支付
func TestExpiredThenLatePayment(t *testing.T) {
	e := newEnv(t)
	d := e.create("M-exp-1", "USDT", "50", e.cbS.URL)
	oid := d["order_id"].(string)
	if _, err := e.app.st.db.Exec(`UPDATE orders SET expires_at=? WHERE id=?`, nowMs()-1000, oid); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(oid, "expired", 10*time.Second)
	cb := e.waitCallback(10 * time.Second)
	var payload map[string]any
	json.Unmarshal(cb.body, &payload)
	if payload["event"] != "expired" {
		t.Fatalf("应先收到 expired 回调: %v", payload)
	}
	e.mock.inject(mkTxn(nextBinanceOrderID(), d["pay_amount"].(string), "USDT", ""))
	e.waitStatus(oid, "paid", 10*time.Second)
}

// 鉴权与幂等
func TestAuthAndDuplicate(t *testing.T) {
	e := newEnv(t)
	body := []byte(`{"merchant_order_id":"M-auth-1","currency":"USDT","amount":"1"}`)
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/orders", bytes.NewReader(body))
	req.Header.Set("X-BPG-Timestamp", strconv.FormatInt(nowMs(), 10))
	req.Header.Set("X-BPG-Nonce", newNonce())
	req.Header.Set("X-BPG-Signature", "bad")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("坏签名应 401，得到 %d", resp.StatusCode)
	}
	resp.Body.Close()
	e.create("M-dup-1", "USDT", "2", "")
	b2, _ := json.Marshal(map[string]any{"merchant_order_id": "M-dup-1", "currency": "USDT", "amount": "2"})
	code, out := e.doSigned("POST", "/api/v1/orders", b2)
	if code != 409 || out["code"] != "ERR_DUPLICATE" {
		t.Fatalf("重复单号应 409/ERR_DUPLICATE，得到 %d %v", code, out)
	}
}

// 唯一金额分配：不撞、关单冷却不复用
func TestAllocatorUniqueness(t *testing.T) {
	e := newEnv(t)
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		d := e.create(fmt.Sprintf("M-alloc-%d", i), "USDT", "88", "")
		pa := d["pay_amount"].(string)
		if seen[pa] {
			t.Fatalf("pay_amount 重复: %s", pa)
		}
		seen[pa] = true
	}
	d := e.create("M-alloc-close", "USDT", "77", "")
	closedAmt := d["pay_amount"].(string)
	if code, out := e.doSigned("POST", "/api/v1/orders/"+d["order_id"].(string)+"/close", nil); code != 200 {
		t.Fatalf("关单失败: %v", out)
	}
	for i := 0; i < 20; i++ {
		d2 := e.create(fmt.Sprintf("M-alloc2-%d", i), "USDT", "77", "")
		if d2["pay_amount"].(string) == closedAmt {
			t.Fatalf("冷却期内金额被复用: %s", closedAmt)
		}
	}
}

// 收银页内的异步请求必须是带 token 的绝对路径：/pay/{token} 无尾斜杠，相对路径会解析到 /pay/status 而 404
func TestCashierAbsolutePaths(t *testing.T) {
	e := newEnv(t)
	d := e.create("M-cashier-1", "USDT", "3", "")
	token := d["pay_url"].(string)[len("http://gw.test/pay/"):]
	resp, err := http.Get(e.srv.URL + "/pay/" + token)
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"/pay/\"+token+\"/status", "/pay/\"+token+\"/claim"} {
		if !strings.Contains(string(html), want) {
			t.Fatalf("收银页缺少绝对路径 %q", want)
		}
	}
	resp, err = http.Get(e.srv.URL + "/pay/" + token + "/status")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("状态接口不可达: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
}

// 多账号：动态添加收款账号，订单按账号隔离匹配，收银页按账号展示，回填也按账号找流水
func TestMultiAccount(t *testing.T) {
	e := newEnv(t)
	post := func(path string, v map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(v)
		return e.doSigned("POST", path, b)
	}
	// 校验失败的 Key / 非法 uid
	if code, out := post("/api/v1/accounts", map[string]any{"api_key": "bad", "api_secret": "s", "uid": "1"}); code != 400 || out["code"] != "ERR_BINANCE" {
		t.Fatalf("坏 Key 应 ERR_BINANCE: %d %v", code, out)
	}
	if code, _ := post("/api/v1/accounts", map[string]any{"api_key": "k2", "api_secret": "s2", "uid": "abc"}); code != 400 {
		t.Fatalf("非法 uid 应 400: %d", code)
	}
	// 添加成功 + 幂等更新
	const k2 = "k2-0123456789abcdef"
	code, out := post("/api/v1/accounts", map[string]any{"label": "sub", "api_key": k2, "api_secret": "s2", "uid": "90000002", "receive_link": "https://app.binance.com/uni-qr/SUB"})
	if code != 200 {
		t.Fatalf("添加账号失败: %d %v", code, out)
	}
	acc := out["data"].(map[string]any)
	accID := acc["account_id"].(string)
	if acc["uid"] != "90000002" || acc["status"] != "active" || acc["api_key_masked"] != "k2-0…cdef" {
		t.Fatalf("账号信息错误: %v", acc)
	}
	code, out = post("/api/v1/accounts", map[string]any{"label": "sub2", "api_key": k2, "api_secret": "s2", "uid": "90000002", "receive_link": "https://app.binance.com/uni-qr/SUB"})
	if code != 200 || out["data"].(map[string]any)["account_id"] != accID || out["data"].(map[string]any)["label"] != "sub2" {
		t.Fatalf("同 Key 应幂等更新: %d %v", code, out)
	}
	var enc string
	if err := e.app.st.db.QueryRow(`SELECT api_secret FROM accounts WHERE id=?`, accID).Scan(&enc); err != nil || !strings.HasPrefix(enc, "v1:") || strings.Contains(enc, "s2") {
		t.Fatalf("密钥应加密落库: %q %v", enc, err)
	}
	// 无效 account_id 下单
	if code, _ := post("/api/v1/orders", map[string]any{"merchant_order_id": "M-acc-bad", "currency": "USDT", "amount": "5", "account_id": "nope"}); code != 400 {
		t.Fatalf("无效账号下单应 400: %d", code)
	}
	// 默认账号一单（让默认账号也在轮询），子账号一单
	e.create("M-acc-def", "USDT", "7", "")
	code, out = post("/api/v1/orders", map[string]any{"merchant_order_id": "M-acc-1", "currency": "USDT", "amount": "5", "account_id": accID, "callback_url": e.cbS.URL})
	if code != 200 {
		t.Fatalf("子账号下单失败: %d %v", code, out)
	}
	d := out["data"].(map[string]any)
	if d["account_id"] != accID || d["receive_uid"] != "90000002" || d["receive_link"] != "https://app.binance.com/uni-qr/SUB" {
		t.Fatalf("订单应带子账号信息: %v", d)
	}
	oid, payAmount := d["order_id"].(string), d["pay_amount"].(string)
	token := d["pay_url"].(string)[len("http://gw.test/pay/"):]
	resp, err := http.Get(e.srv.URL + "/pay/" + token)
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(html), "90000002") || !strings.Contains(string(html), "扫码支付") {
		i := strings.Index(string(html), "收款人币安 UID")
		seg := ""
		if i >= 0 {
			seg = string(html)[i : i+160]
		}
		t.Fatalf("收银页应展示子账号 UID 与扫码: HTTP %d uid=%v qr=%v seg=%q", resp.StatusCode, strings.Contains(string(html), "90000002"), strings.Contains(string(html), "扫码支付"), seg)
	}
	if resp, err = http.Get(e.srv.URL + "/pay/" + token + "/qr"); err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("子账号二维码不可用: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	// 隔离：默认账号收到同金额流水，不应核销子账号订单
	e.mock.inject(mkTxn(nextBinanceOrderID(), payAmount, "USDT", ""))
	time.Sleep(2500 * time.Millisecond)
	if _, out := e.doSigned("GET", "/api/v1/orders/"+oid, nil); out["data"].(map[string]any)["status"] != "pending" {
		t.Fatalf("默认账号流水不应核销子账号订单: %v", out)
	}
	// 子账号收到 → paid，回调带 account_id
	bid := nextBinanceOrderID()
	e.mock.injectFor(k2, mkTxn(bid, payAmount, "USDT", ""))
	got := e.waitStatus(oid, "paid", 10*time.Second)
	if got["binance_order_id"] != bid || got["matched_by"] != "amount" {
		t.Fatalf("子账号匹配信息错误: %v", got)
	}
	cb := e.waitCallback(10 * time.Second)
	var payload map[string]any
	json.Unmarshal(cb.body, &payload)
	if payload["account_id"] != accID || payload["event"] != "paid" {
		t.Fatalf("回调应带 account_id: %v", payload)
	}
	if _, out := e.doSigned("GET", "/api/v1/accounts/"+accID, nil); out["data"].(map[string]any)["last_ok"].(float64) <= 0 {
		t.Fatalf("轮询成功后 last_ok 应更新: %v", out)
	}
	// 回填：子账号第二单，金额=基础值无备注 → 收银页回填订单编号 → 在子账号流水里找到
	code, out = post("/api/v1/orders", map[string]any{"merchant_order_id": "M-acc-2", "currency": "USDT", "amount": "9", "account_id": accID})
	d2 := out["data"].(map[string]any)
	bid2 := nextBinanceOrderID()
	e.mock.injectFor(k2, mkTxn(bid2, "9", "USDT", ""))
	tok2 := d2["pay_url"].(string)[len("http://gw.test/pay/"):]
	cbody, _ := json.Marshal(map[string]string{"binance_order_id": bid2})
	resp, err = http.Post(e.srv.URL+"/pay/"+tok2+"/claim", "application/json", bytes.NewReader(cbody))
	if err != nil {
		t.Fatal(err)
	}
	var cres map[string]any
	json.NewDecoder(resp.Body).Decode(&cres)
	resp.Body.Close()
	if cres["data"].(map[string]any)["code"] != "OK" {
		t.Fatalf("子账号回填失败: %v", cres)
	}
	// 停用 / 启用 / 默认账号保护
	if code, out := post("/api/v1/accounts/"+accID+"/disable", nil); code != 200 || out["data"].(map[string]any)["status"] != "disabled" {
		t.Fatalf("停用失败: %d %v", code, out)
	}
	if code, _ := post("/api/v1/orders", map[string]any{"merchant_order_id": "M-acc-3", "currency": "USDT", "amount": "5", "account_id": accID}); code != 400 {
		t.Fatalf("停用账号下单应 400: %d", code)
	}
	if code, _ := post("/api/v1/accounts/"+accID+"/enable", nil); code != 200 {
		t.Fatalf("启用失败: %d", code)
	}
	if code, _ := post("/api/v1/accounts/default/disable", nil); code != 400 {
		t.Fatalf("默认账号不可停用: %d", code)
	}
	if code, out := post("/api/v1/accounts/default/verify", nil); code != 200 || out["data"].(map[string]any)["uid"] != "90000001" {
		t.Fatalf("默认账号校验: %d %v", code, out)
	}
}
