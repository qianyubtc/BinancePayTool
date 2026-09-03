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

type mockBinance struct {
	mu   sync.Mutex
	txns []payTxn
	srv  *httptest.Server
}

func newMockBinance() *mockBinance {
	m := &mockBinance{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sapi/v1/pay/transactions", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		writeJSON(w, 200, map[string]any{"code": "000000", "message": "success", "data": m.txns, "success": true})
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockBinance) inject(t payTxn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.txns = append(m.txns, t)
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
