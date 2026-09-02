package main

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Matcher 负责轮询币安流水并按三条规则核销订单：
// ① 唯一金额精确匹配 ② 备注含 note_code ③ 收银页回填币安订单编号（claim）。
type Matcher struct {
	cfg *Config
	st  *Store
	bc  *binanceClient

	mu      sync.Mutex
	cache   []payTxn
	cacheAt int64
}

func newMatcher(cfg *Config, st *Store, bc *binanceClient) *Matcher {
	return &Matcher{cfg: cfg, st: st, bc: bc}
}

func (m *Matcher) run(ctx context.Context) {
	t := time.NewTicker(time.Duration(m.cfg.PollInterval) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := m.st.ExpireDue(m.cfg.SuffixCooldown); err != nil {
				log.Printf("[error] 过期处理失败: %v", err)
			} else if n > 0 {
				log.Printf("[info] %d 个订单已过期", n)
			}
			if err := m.scan(); err != nil {
				log.Printf("[error] 流水扫描失败: %v", err)
			}
		}
	}
}

// fetchWindow 拉取自 startMs 以来的流水，满 100 条时向前翻页（最多 5 页），按时间升序返回。
func (m *Matcher) fetchWindow(startMs int64) ([]payTxn, error) {
	seen := map[string]bool{}
	var all []payTxn
	endMs := int64(0)
	for page := 0; page < 5; page++ {
		txns, err := m.bc.PayTransactions(startMs, endMs, 100)
		if err != nil {
			return nil, err
		}
		minT := int64(0)
		for _, t := range txns {
			key := t.OrderID
			if key == "" {
				key = t.TransactionID
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, t)
			if minT == 0 || t.Time < minT {
				minT = t.Time
			}
		}
		if len(txns) < 100 || minT == 0 {
			break
		}
		endMs = minT - 1
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Time < all[j].Time })
	return all, nil
}

func (m *Matcher) scan() error {
	now := nowMs()
	graceMs := m.cfg.ExpiredGrace * 1000
	cands, err := m.st.CandidateOrders(now, graceMs)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return nil
	}
	minCreated := cands[0].CreatedAt
	for _, o := range cands {
		if o.CreatedAt < minCreated {
			minCreated = o.CreatedAt
		}
	}
	txns, err := m.fetchWindow(minCreated - 10*60*1000)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cache, m.cacheAt = txns, now
	m.mu.Unlock()
	for _, t := range txns {
		if err := m.applyTxn(t); err != nil {
			log.Printf("[error] 核销流水 %s 失败: %v", t.OrderID, err)
		}
	}
	return nil
}

// applyTxn 尝试用一笔流水核销订单（规则 ①②）。
func (m *Matcher) applyTxn(t payTxn) error {
	amt, err := parseAmount(t.Amount)
	if err != nil || amt <= 0 {
		return nil // 非入账
	}
	cur := strings.ToUpper(t.Currency)
	if !m.cfg.Currencies[cur] {
		return nil
	}
	if t.OrderID == "" {
		return nil
	}
	if done, err := m.st.Consumed(t.OrderID); err != nil || done {
		return err
	}
	now := nowMs()
	cands, err := m.st.CandidateOrders(now, m.cfg.ExpiredGrace*1000)
	if err != nil {
		return err
	}
	payerID := strconv64(t.CounterpartyID)
	if t.PayerInfo.BinanceID > 0 {
		payerID = strconv64(t.PayerInfo.BinanceID)
	}
	// ① 唯一金额精确匹配
	for _, o := range cands {
		if o.Currency == cur && o.PayAmount == amt && t.Time >= o.CreatedAt-120*1000 {
			ok, err := m.st.Finish(o, "paid", "amount", t.OrderID, t.TransactionID, payerID, t.PayerInfo.Name, amt, m.cfg.SuffixCooldown)
			if ok {
				log.Printf("[info] 订单 %s 已支付（金额匹配）binanceOrderId=%s amount=%s %s", o.ID, t.OrderID, fmtAmount(amt), cur)
			}
			return err
		}
	}
	// ② 备注含 note_code
	note := strings.ToUpper(t.Note)
	if note == "" {
		return nil
	}
	for _, o := range cands {
		if o.Currency != cur || !strings.Contains(note, o.NoteCode) {
			continue
		}
		status := "paid"
		if amt < o.BaseAmount {
			status = "underpaid"
		}
		ok, err := m.st.Finish(o, status, "note", t.OrderID, t.TransactionID, payerID, t.PayerInfo.Name, amt, m.cfg.SuffixCooldown)
		if ok {
			log.Printf("[info] 订单 %s → %s（备注匹配）binanceOrderId=%s amount=%s %s", o.ID, status, t.OrderID, fmtAmount(amt), cur)
		}
		return err
	}
	return nil
}

type claimResult struct {
	Code   string // OK / NOT_FOUND / CONSUMED / CURRENCY / UNDERPAID / STATE
	Status string // 处理后订单状态
}

// Claim 处理收银页回填的币安订单编号（规则 ③）。
func (m *Matcher) Claim(o *Order, binanceOrderID string) (claimResult, error) {
	if o.Status == "paid" {
		return claimResult{Code: "OK", Status: "paid"}, nil
	}
	if o.Status != "pending" && o.Status != "expired" {
		return claimResult{Code: "STATE", Status: o.Status}, nil
	}
	if done, err := m.st.Consumed(binanceOrderID); err != nil {
		return claimResult{}, err
	} else if done {
		return claimResult{Code: "CONSUMED", Status: o.Status}, nil
	}
	// 先查缓存；未命中则强制现拉一次（付款可能刚到账，缓存虽新但没有它）
	find := func(txns []payTxn) *payTxn {
		for i := range txns {
			if txns[i].OrderID == binanceOrderID {
				return &txns[i]
			}
		}
		return nil
	}
	m.mu.Lock()
	hit := find(m.cache)
	m.mu.Unlock()
	if hit == nil {
		fresh, err := m.fetchWindow(o.CreatedAt - 10*60*1000)
		if err != nil {
			return claimResult{}, err
		}
		m.mu.Lock()
		m.cache, m.cacheAt = fresh, nowMs()
		m.mu.Unlock()
		hit = find(fresh)
	}
	if hit != nil {
		t := *hit
		amt, err := parseAmount(t.Amount)
		if err != nil || amt <= 0 {
			return claimResult{Code: "NOT_FOUND", Status: o.Status}, nil
		}
		if strings.ToUpper(t.Currency) != o.Currency {
			return claimResult{Code: "CURRENCY", Status: o.Status}, nil
		}
		payerID := strconv64(t.CounterpartyID)
		if t.PayerInfo.BinanceID > 0 {
			payerID = strconv64(t.PayerInfo.BinanceID)
		}
		status := "paid"
		code := "OK"
		if amt < o.BaseAmount {
			status = "underpaid"
			code = "UNDERPAID"
		}
		ok, err := m.st.Finish(o, status, "claim", t.OrderID, t.TransactionID, payerID, t.PayerInfo.Name, amt, m.cfg.SuffixCooldown)
		if err != nil {
			return claimResult{}, err
		}
		if !ok {
			return claimResult{Code: "CONSUMED", Status: o.Status}, nil
		}
		log.Printf("[info] 订单 %s → %s（回填匹配）binanceOrderId=%s", o.ID, status, t.OrderID)
		return claimResult{Code: code, Status: status}, nil
	}
	return claimResult{Code: "NOT_FOUND", Status: o.Status}, nil
}

func strconv64(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}
