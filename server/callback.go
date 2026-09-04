package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

var cbSchedule = []int64{0, 15, 60, 300, 900, 3600, 10800} // 秒

type Notifier struct {
	cfg *Config
	st  *Store
	hc  *http.Client
}

func newNotifier(cfg *Config, st *Store) *Notifier {
	return &Notifier{cfg: cfg, st: st, hc: &http.Client{Timeout: 10 * time.Second}}
}

func (n *Notifier) run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			jobs, err := n.st.DueCallbacks(10)
			if err != nil {
				log.Printf("[error] 读取回调队列失败: %v", err)
				continue
			}
			for _, j := range jobs {
				n.deliver(j)
			}
		}
	}
}

func callbackPayload(o *Order, event string) ([]byte, error) {
	actual := ""
	if o.ActualAmount > 0 {
		actual = fmtAmount(o.ActualAmount)
	}
	p := map[string]any{
		"event":             event,
		"order_id":          o.ID,
		"account_id":        o.AccountID,
		"merchant_order_id": o.MerchantOrderID,
		"status":            o.Status,
		"currency":          o.Currency,
		"base_amount":       fmtAmount(o.BaseAmount),
		"pay_amount":        fmtAmount(o.PayAmount),
		"actual_amount":     actual,
		"overpaid":          o.Overpaid,
		"matched_by":        o.MatchedBy,
		"binance_order_id":  o.BinanceOrderID,
		"binance_txn_id":    o.BinanceTxnID,
		"payer_id":          o.PayerID,
		"paid_at":           o.PaidAt,
		"timestamp":         nowMs(),
	}
	return json.Marshal(p)
}

func (n *Notifier) deliver(j cbJob) {
	o, err := n.st.OrderByID(j.OrderID)
	if err != nil {
		log.Printf("[error] 回调找不到订单 %s: %v", j.OrderID, err)
		n.st.CallbackDone(j.ID)
		return
	}
	if o.CallbackURL == "" {
		n.st.CallbackDone(j.ID)
		return
	}
	body, err := callbackPayload(o, j.Event)
	if err != nil {
		n.st.CallbackDone(j.ID)
		return
	}
	ts := strconv.FormatInt(nowMs(), 10)
	nonce := newNonce()
	req, err := http.NewRequest("POST", o.CallbackURL, bytes.NewReader(body))
	if err != nil {
		n.retry(j, fmt.Sprintf("bad url: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BPG-Timestamp", ts)
	req.Header.Set("X-BPG-Nonce", nonce)
	req.Header.Set("X-BPG-Signature", signCallback(n.cfg.APIAuthKey, ts, nonce, body))
	resp, err := n.hc.Do(req)
	if err != nil {
		n.retry(j, err.Error())
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		n.st.CallbackDone(j.ID)
		log.Printf("[info] 回调成功 order=%s event=%s → %s", o.ID, j.Event, o.CallbackURL)
		return
	}
	n.retry(j, fmt.Sprintf("HTTP %d", resp.StatusCode))
}

func (n *Notifier) retry(j cbJob, errStr string) {
	next := j.Attempt + 1
	if next >= len(cbSchedule) {
		n.st.CallbackRetry(j.ID, next, 0, errStr, true)
		log.Printf("[warn] 回调放弃 order=%s attempts=%d lastErr=%s", j.OrderID, next, errStr)
		return
	}
	n.st.CallbackRetry(j.ID, next, nowMs()+cbSchedule[next]*1000, errStr, false)
	log.Printf("[warn] 回调失败将重试 order=%s attempt=%d err=%s", j.OrderID, next, errStr)
}
