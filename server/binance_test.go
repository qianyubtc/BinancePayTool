package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// clockMock 模拟币安签名接口：时间戳偏差超过 recvWindow 返回 -1021。
type clockMock struct {
	mu       sync.Mutex
	payHits  int
	timeHits int
}

func (m *clockMock) server(aheadMs int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/time":
			m.mu.Lock()
			m.timeHits++
			m.mu.Unlock()
			writeJSON(w, 200, map[string]any{"serverTime": nowMs() + aheadMs})
		case "/sapi/v1/pay/transactions":
			m.mu.Lock()
			m.payHits++
			m.mu.Unlock()
			ts, _ := strconv.ParseInt(r.URL.Query().Get("timestamp"), 10, 64)
			if diff := ts - (nowMs() + aheadMs); diff > 10_000 || diff < -10_000 {
				writeJSON(w, 400, map[string]any{"code": -1021, "msg": "Timestamp for this request is outside of the recvWindow."})
				return
			}
			writeJSON(w, 200, map[string]any{
				"success": true,
				"data": []payTxn{{
					OrderID: "452000000000000001", Amount: "1", Currency: "USDT", Time: nowMs(),
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// 本地时钟慢 60 秒：首次请求 -1021 → /api/v3/time 校时 → 重试成功。
func TestPayTransactionsResyncsClockOn1021(t *testing.T) {
	m := &clockMock{}
	srv := m.server(60_000)
	defer srv.Close()
	bc := newBinanceClientFor(srv.URL, "k", "s")
	txns, err := bc.PayTransactions(0, 0, 100)
	if err != nil {
		t.Fatalf("校时后应成功: %v", err)
	}
	if len(txns) != 1 || txns[0].OrderID != "452000000000000001" {
		t.Fatalf("流水内容错误: %+v", txns)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.payHits != 2 || m.timeHits != 1 {
		t.Fatalf("请求次数 pay=%d time=%d，期望 2/1", m.payHits, m.timeHits)
	}
}

// 其他错误码（如 -2015）不触发校时，也不重试。
func TestPayTransactionsDoesNotResyncOnOtherErrors(t *testing.T) {
	var mu sync.Mutex
	timeHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/time" {
			mu.Lock()
			timeHits++
			mu.Unlock()
			writeJSON(w, 200, map[string]any{"serverTime": nowMs()})
			return
		}
		writeJSON(w, 401, map[string]any{"code": -2015, "msg": "Invalid API-key, IP, or permissions for action."})
	}))
	defer srv.Close()
	bc := newBinanceClientFor(srv.URL, "bad", "s")
	_, err := bc.PayTransactions(0, 0, 1)
	var be *binanceError
	if !errors.As(err, &be) || be.code != -2015 {
		t.Fatalf("应返回 -2015 业务错误，得到 %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if timeHits != 0 {
		t.Fatalf("非 -1021 不应校时，实际调用 %d 次", timeHits)
	}
}

func newRows(total int, stepMs, base int64) []payTxn {
	rows := make([]payTxn, total)
	for i := range rows {
		rows[i] = payTxn{
			OrderID:  strconv.FormatInt(int64(452000000000000000)+int64(i), 10),
			Amount:   "1",
			Currency: "USDT",
			Time:     base + int64(i)*stepMs,
		}
	}
	return rows
}

// newPageMock 按 startTime/endTime 过滤、升序返回，最多 100 条（模拟最坏的返回顺序）。
func newPageMock(rows []payTxn) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
		out := make([]payTxn, 0, fetchPageLimit)
		for _, row := range rows {
			if row.Time < start {
				continue
			}
			if end > 0 && row.Time > end {
				continue
			}
			out = append(out, row)
			if len(out) == fetchPageLimit {
				break
			}
		}
		writeJSON(w, 200, map[string]any{"success": true, "data": out})
	}))
}

// 升序返回时旧翻页逻辑只看得到最早一页；二分切窗必须拿全 150 条。
func TestFetchWindowSplitsFullPages(t *testing.T) {
	const total = 150
	base := nowMs() - int64(total)*1000
	rows := newRows(total, 1000, base)
	srv := newPageMock(rows)
	defer srv.Close()

	m := newMatcher(&Config{}, nil)
	as := &accountState{bc: newBinanceClientFor(srv.URL, "k", "s")}
	got, err := m.fetchWindow(as, base-1000)
	if err != nil {
		t.Fatalf("fetchWindow: %v", err)
	}
	if len(got) != total {
		t.Fatalf("拿到 %d 条，期望 %d", len(got), total)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Fatalf("未按时间升序: %d < %d", got[i].Time, got[i-1].Time)
		}
	}
}

// 同一毫秒超过一页：无法再二分，必须报错而不是静默漏单。
func TestFetchWindowFailsLoudlyOnUnsplitTruncation(t *testing.T) {
	const total = 101
	same := nowMs() - 60_000
	rows := newRows(total, 0, same)
	srv := newPageMock(rows)
	defer srv.Close()

	m := newMatcher(&Config{}, nil)
	as := &accountState{bc: newBinanceClientFor(srv.URL, "k", "s")}
	if _, err := m.fetchWindow(as, same); err == nil {
		t.Fatal("同毫秒超过一页应报错，不能静默漏单")
	}
}
