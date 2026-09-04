package main

import (
	"context"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultAccountID = "default"

// defaultAccount 把 config.env 里的收款账号包装成 Account，与动态账号统一处理。
func defaultAccount(cfg *Config) *Account {
	return &Account{ID: defaultAccountID, Label: "default", APIKey: cfg.BinanceKey, APISecret: cfg.BinanceSecret, UID: cfg.BinanceUID,
		ReceiveLink: cfg.ReceiveLink, ReceiveEmail: cfg.ReceiveEmail, Status: "active"}
}

// accountState 一个收款账号的轮询状态：币安客户端 + 最近一次拉到的流水缓存。
type accountState struct {
	acc         *Account
	bc          *binanceClient
	cache       []payTxn
	cacheAt     int64
	lastOKWrite int64
	lastErr     string
}

// Matcher 按收款账号轮询币安流水并按三条规则核销该账号的订单：
// ① 唯一金额精确匹配 ② 备注含 note_code ③ 收银页回填币安订单编号（claim）。
// 不同账号的流水与订单严格隔离。
type Matcher struct {
	cfg  *Config
	st   *Store
	mu   sync.Mutex
	accs map[string]*accountState
}

func newMatcher(cfg *Config, st *Store) *Matcher {
	m := &Matcher{cfg: cfg, st: st, accs: map[string]*accountState{}}
	m.accs[defaultAccountID] = &accountState{acc: defaultAccount(cfg), bc: newBinanceClient(cfg)}
	return m
}

// refresh 同步数据库里的账号：新增的建状态，停用的移除，密钥变更的重建客户端。
func (m *Matcher) refresh() []*accountState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if list, err := m.st.ListAccounts(true); err != nil {
		log.Printf("[error] 读取账号列表失败: %v", err)
	} else {
		live := map[string]bool{defaultAccountID: true}
		for _, a := range list {
			live[a.ID] = true
			as, ok := m.accs[a.ID]
			if !ok || as.acc.APIKey != a.APIKey || as.acc.APISecret != a.APISecret {
				as = &accountState{bc: newBinanceClientFor(m.cfg.BinanceAPIBase, a.APIKey, a.APISecret)}
				m.accs[a.ID] = as
			}
			as.acc = a
		}
		for id := range m.accs {
			if !live[id] {
				delete(m.accs, id)
			}
		}
	}
	out := make([]*accountState, 0, len(m.accs))
	for _, as := range m.accs {
		out = append(out, as)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].acc.ID < out[j].acc.ID })
	return out
}

// stateFor 取某账号的状态；刚创建还没被 refresh 到的账号即时加载。
func (m *Matcher) stateFor(id string) *accountState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if as, ok := m.accs[id]; ok {
		return as
	}
	if id != defaultAccountID {
		if a, err := m.st.GetAccount(id); err == nil && a.Status == "active" {
			as := &accountState{acc: a, bc: newBinanceClientFor(m.cfg.BinanceAPIBase, a.APIKey, a.APISecret)}
			m.accs[id] = as
			return as
		}
	}
	return nil
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
			for _, as := range m.refresh() {
				polled, err := m.scan(as)
				m.record(as, polled, err)
			}
		}
	}
}

// record 把轮询结果写回账号表：成功每分钟最多写一次，错误只在内容变化时写。
func (m *Matcher) record(as *accountState, polled bool, err error) {
	if err != nil {
		log.Printf("[error] 账号 %s 流水扫描失败: %v", as.acc.ID, err)
	}
	if as.acc.ID == defaultAccountID || !polled {
		return
	}
	now := nowMs()
	if err != nil {
		if msg := truncate(err.Error(), 200); msg != as.lastErr {
			as.lastErr = msg
			m.st.AccountPolled(as.acc.ID, false, msg)
		}
		return
	}
	if as.lastErr != "" || now-as.lastOKWrite > 60000 {
		as.lastErr = ""
		as.lastOKWrite = now
		m.st.AccountPolled(as.acc.ID, true, "")
	}
}

// fetchWindow 拉取自 startMs 以来的流水，满 100 条时向前翻页（最多 5 页），按时间升序返回。
func (m *Matcher) fetchWindow(as *accountState, startMs int64) ([]payTxn, error) {
	seen := map[string]bool{}
	var all []payTxn
	endMs := int64(0)
	for page := 0; page < 5; page++ {
		txns, err := as.bc.PayTransactions(startMs, endMs, 100)
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

// scan 扫描一个账号：没有待核销订单就不调币安接口（省权重）。返回是否真的拉取了流水。
func (m *Matcher) scan(as *accountState) (bool, error) {
	now := nowMs()
	graceMs := m.cfg.ExpiredGrace * 1000
	cands, err := m.st.CandidateOrders(as.acc.ID, now, graceMs)
	if err != nil {
		return false, err
	}
	if len(cands) == 0 {
		return false, nil
	}
	minCreated := cands[0].CreatedAt
	for _, o := range cands {
		if o.CreatedAt < minCreated {
			minCreated = o.CreatedAt
		}
	}
	txns, err := m.fetchWindow(as, minCreated-10*60*1000)
	if err != nil {
		return true, err
	}
	m.mu.Lock()
	as.cache, as.cacheAt = txns, now
	m.mu.Unlock()
	for _, t := range txns {
		if err := m.applyTxn(as, t); err != nil {
			log.Printf("[error] 核销流水 %s 失败: %v", t.OrderID, err)
		}
	}
	return true, nil
}

func payerOf(t payTxn) string {
	if t.PayerInfo.BinanceID > 0 {
		return strconv64(t.PayerInfo.BinanceID)
	}
	return strconv64(t.CounterpartyID)
}

// applyTxn 尝试用一笔流水核销该账号的订单（规则 ①②）。
func (m *Matcher) applyTxn(as *accountState, t payTxn) error {
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
	cands, err := m.st.CandidateOrders(as.acc.ID, now, m.cfg.ExpiredGrace*1000)
	if err != nil {
		return err
	}
	payerID := payerOf(t)
	// ① 唯一金额精确匹配
	for _, o := range cands {
		if o.Currency == cur && o.PayAmount == amt && t.Time >= o.CreatedAt-120*1000 {
			ok, err := m.st.Finish(o, "paid", "amount", t.OrderID, t.TransactionID, payerID, t.PayerInfo.Name, amt, m.cfg.SuffixCooldown)
			if ok {
				log.Printf("[info] 订单 %s 已支付（金额匹配）account=%s binanceOrderId=%s amount=%s %s", o.ID, as.acc.ID, t.OrderID, fmtAmount(amt), cur)
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
			log.Printf("[info] 订单 %s → %s（备注匹配）account=%s binanceOrderId=%s amount=%s %s", o.ID, status, as.acc.ID, t.OrderID, fmtAmount(amt), cur)
		}
		return err
	}
	return nil
}

type claimResult struct {
	Code   string // OK / NOT_FOUND / CONSUMED / CURRENCY / UNDERPAID / STATE
	Status string // 处理后订单状态
}

// Claim 处理收银页回填的币安订单编号（规则 ③），只在订单所属账号的流水里找。
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
	as := m.stateFor(o.AccountID)
	if as == nil {
		return claimResult{}, errors.New("订单所属收款账号不可用")
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
	hit := find(as.cache)
	m.mu.Unlock()
	if hit == nil {
		fresh, err := m.fetchWindow(as, o.CreatedAt-10*60*1000)
		if err != nil {
			return claimResult{}, err
		}
		m.mu.Lock()
		as.cache, as.cacheAt = fresh, nowMs()
		m.mu.Unlock()
		hit = find(fresh)
	}
	if hit == nil {
		return claimResult{Code: "NOT_FOUND", Status: o.Status}, nil
	}
	t := *hit
	amt, err := parseAmount(t.Amount)
	if err != nil || amt <= 0 {
		return claimResult{Code: "NOT_FOUND", Status: o.Status}, nil
	}
	if strings.ToUpper(t.Currency) != o.Currency {
		return claimResult{Code: "CURRENCY", Status: o.Status}, nil
	}
	status, code := "paid", "OK"
	if amt < o.BaseAmount {
		status, code = "underpaid", "UNDERPAID"
	}
	ok, err := m.st.Finish(o, status, "claim", t.OrderID, t.TransactionID, payerOf(t), t.PayerInfo.Name, amt, m.cfg.SuffixCooldown)
	if err != nil {
		return claimResult{}, err
	}
	if !ok {
		return claimResult{Code: "CONSUMED", Status: o.Status}, nil
	}
	log.Printf("[info] 订单 %s → %s（回填匹配）account=%s binanceOrderId=%s", o.ID, status, o.AccountID, t.OrderID)
	return claimResult{Code: code, Status: status}, nil
}

func strconv64(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}
