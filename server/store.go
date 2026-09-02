package main

import (
	"database/sql"
	"errors"
	"math/rand"
	"sync"

	_ "modernc.org/sqlite"
)

var (
	ErrDuplicate = errors.New("duplicate merchant_order_id")
	ErrAlloc     = errors.New("no unique amount available")
	ErrNotFound  = errors.New("not found")
	ErrState     = errors.New("invalid state")
)

type Order struct {
	ID              string
	Token           string
	MerchantOrderID string
	Currency        string
	BaseAmount      int64
	PayAmount       int64
	ActualAmount    int64
	Status          string // pending|paid|underpaid|expired|closed
	NoteCode        string
	BinanceOrderID  string
	BinanceTxnID    string
	PayerID         string
	PayerName       string
	MatchedBy       string // amount|note|claim
	CallbackURL     string
	ReturnURL       string
	CreatedAt       int64
	ExpiresAt       int64
	PaidAt          int64
	Overpaid        bool
}

type cbJob struct {
	ID      int64
	OrderID string
	Event   string
	Attempt int
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS orders(
  id TEXT PRIMARY KEY,
  token TEXT UNIQUE NOT NULL,
  merchant_order_id TEXT UNIQUE NOT NULL,
  currency TEXT NOT NULL,
  base_amount INTEGER NOT NULL,
  pay_amount INTEGER NOT NULL,
  actual_amount INTEGER,
  status TEXT NOT NULL,
  note_code TEXT NOT NULL,
  binance_order_id TEXT,
  binance_txn_id TEXT,
  payer_id TEXT,
  payer_name TEXT,
  matched_by TEXT,
  callback_url TEXT,
  return_url TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  paid_at INTEGER,
  overpaid INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_amount ON orders(currency, pay_amount);
CREATE TABLE IF NOT EXISTS consumed(
  binance_order_id TEXT PRIMARY KEY,
  order_id TEXT NOT NULL,
  at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS cooldown(
  currency TEXT NOT NULL,
  amount INTEGER NOT NULL,
  until INTEGER NOT NULL,
  PRIMARY KEY(currency, amount)
);
CREATE TABLE IF NOT EXISTS callbacks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id TEXT NOT NULL,
  event TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER NOT NULL,
  done INTEGER NOT NULL DEFAULT 0,
  last_err TEXT
);
CREATE INDEX IF NOT EXISTS idx_callbacks_due ON callbacks(done, next_at);
`

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
	} {
		if _, err := db.Exec(p); err != nil {
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const orderCols = `id,token,merchant_order_id,currency,base_amount,pay_amount,COALESCE(actual_amount,0),status,note_code,
COALESCE(binance_order_id,''),COALESCE(binance_txn_id,''),COALESCE(payer_id,''),COALESCE(payer_name,''),COALESCE(matched_by,''),
COALESCE(callback_url,''),COALESCE(return_url,''),created_at,expires_at,COALESCE(paid_at,0),overpaid`

type rowScanner interface{ Scan(...any) error }

func scanOrder(row rowScanner) (*Order, error) {
	var o Order
	var overpaid int
	err := row.Scan(&o.ID, &o.Token, &o.MerchantOrderID, &o.Currency, &o.BaseAmount, &o.PayAmount, &o.ActualAmount,
		&o.Status, &o.NoteCode, &o.BinanceOrderID, &o.BinanceTxnID, &o.PayerID, &o.PayerName, &o.MatchedBy,
		&o.CallbackURL, &o.ReturnURL, &o.CreatedAt, &o.ExpiresAt, &o.PaidAt, &overpaid)
	if err != nil {
		return nil, err
	}
	o.Overpaid = overpaid != 0
	return &o, nil
}

func (s *Store) orderBy(where string, arg any) (*Order, error) {
	row := s.db.QueryRow(`SELECT `+orderCols+` FROM orders WHERE `+where, arg)
	o, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

func (s *Store) OrderByID(id string) (*Order, error)     { return s.orderBy("id=?", id) }
func (s *Store) OrderByToken(tok string) (*Order, error) { return s.orderBy("token=?", tok) }
func (s *Store) OrderByMerchantID(mid string) (*Order, error) {
	return s.orderBy("merchant_order_id=?", mid)
}

// CandidateOrders 返回可被入账流水核销的订单：待支付，或过期但仍在宽限期内。
func (s *Store) CandidateOrders(now, graceMs int64) ([]*Order, error) {
	rows, err := s.db.Query(`SELECT `+orderCols+` FROM orders
		WHERE status='pending' OR (status='expired' AND expires_at+? > ?)`, graceMs, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CreateOrder 分配唯一金额并落库。分配与插入在同一把锁内，杜绝并发撞金额。
func (s *Store) CreateOrder(cfg *Config, mid, currency string, base int64, ttlSec int64, callbackURL, returnURL string) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowMs()

	var exist string
	err := s.db.QueryRow(`SELECT id FROM orders WHERE merchant_order_id=?`, mid).Scan(&exist)
	if err == nil {
		return nil, ErrDuplicate
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	unit := int64(1)
	for i := 0; i < 8-cfg.AmountDecimals; i++ {
		unit *= 10
	}
	maxK := int64(1)
	for i := 0; i < cfg.AmountDecimals; i++ {
		maxK *= 10
	}
	maxK--

	lo, hi := base+unit, base+maxK*unit
	if cfg.SuffixMode == "sub" {
		lo, hi = base-maxK*unit, base-unit
		if lo < unit {
			lo = unit
		}
		if hi < lo {
			return nil, ErrAlloc
		}
	}

	taken := map[int64]bool{}
	graceMs := cfg.ExpiredGrace * 1000
	rows, err := s.db.Query(`SELECT pay_amount FROM orders WHERE currency=? AND pay_amount BETWEEN ? AND ?
		AND (status='pending' OR (status IN ('expired','underpaid') AND expires_at+? > ?))`,
		currency, lo, hi, graceMs, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		taken[v] = true
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT amount FROM cooldown WHERE currency=? AND amount BETWEEN ? AND ? AND until > ?`,
		currency, lo, hi, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		taken[v] = true
	}
	rows.Close()

	// 递进式尾数：优先最小的两位档（付款方最多多付 0.0099），
	// 该档用满自动升到三位、四位档，兼顾「多付最少」与并发容量。
	candOf := func(k int64) int64 {
		if cfg.SuffixMode == "sub" {
			return base - k*unit
		}
		return base + k*unit
	}
	pick := int64(0)
	rangeLo := int64(1)
	for _, rangeHi := range []int64{99, 999, maxK} {
		if rangeHi > maxK {
			rangeHi = maxK
		}
		if rangeHi < rangeLo {
			break
		}
		span := rangeHi - rangeLo + 1
		for i := int64(0); i < 200 && i < span*2; i++ {
			k := rangeLo + rand.Int63n(span)
			c := candOf(k)
			if c > 0 && !taken[c] {
				pick = c
				break
			}
		}
		if pick == 0 {
			for k := rangeLo; k <= rangeHi; k++ {
				c := candOf(k)
				if c > 0 && !taken[c] {
					pick = c
					break
				}
			}
		}
		if pick != 0 {
			break
		}
		rangeLo = rangeHi + 1
	}
	if pick == 0 {
		return nil, ErrAlloc
	}

	o := &Order{
		ID: newOrderID(), Token: newToken(), MerchantOrderID: mid, Currency: currency,
		BaseAmount: base, PayAmount: pick, Status: "pending", NoteCode: newNoteCode(),
		CallbackURL: callbackURL, ReturnURL: returnURL,
		CreatedAt: now, ExpiresAt: now + ttlSec*1000,
	}
	_, err = s.db.Exec(`INSERT INTO orders(id,token,merchant_order_id,currency,base_amount,pay_amount,status,note_code,callback_url,return_url,created_at,expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.Token, o.MerchantOrderID, o.Currency, o.BaseAmount, o.PayAmount, o.Status, o.NoteCode,
		o.CallbackURL, o.ReturnURL, o.CreatedAt, o.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// Finish 用一笔币安流水核销订单（paid 或 underpaid）。以币安 orderId 幂等：已消费返回 false。
func (s *Store) Finish(o *Order, status, matchedBy, binanceOrderID, binanceTxnID, payerID, payerName string, actual int64, cooldownSec int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowMs()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO consumed(binance_order_id,order_id,at) VALUES(?,?,?)`, binanceOrderID, o.ID, now); err != nil {
		return false, nil // 已被消费（唯一键冲突），不视为错误
	}
	res, err := tx.Exec(`UPDATE orders SET status=?, matched_by=?, binance_order_id=?, binance_txn_id=?, payer_id=?, payer_name=?,
		actual_amount=?, paid_at=?, overpaid=? WHERE id=? AND status IN ('pending','expired')`,
		status, matchedBy, binanceOrderID, binanceTxnID, payerID, payerName,
		actual, now, boolInt(actual > o.PayAmount), o.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil // 订单已被其他流水处理
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO cooldown(currency,amount,until) VALUES(?,?,?)`,
		o.Currency, o.PayAmount, now+cooldownSec*1000); err != nil {
		return false, err
	}
	event := status // paid / underpaid
	if _, err := tx.Exec(`INSERT INTO callbacks(order_id,event,next_at) VALUES(?,?,?)`, o.ID, event, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ExpireDue 把到期订单置为 expired，并入冷却、发回调。返回处理条数。
func (s *Store) ExpireDue(cooldownSec int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowMs()
	rows, err := s.db.Query(`SELECT `+orderCols+` FROM orders WHERE status='pending' AND expires_at < ?`, now)
	if err != nil {
		return 0, err
	}
	var due []*Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, o)
	}
	rows.Close()
	for _, o := range due {
		tx, err := s.db.Begin()
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE orders SET status='expired' WHERE id=? AND status='pending'`, o.ID); err != nil {
			tx.Rollback()
			return 0, err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO cooldown(currency,amount,until) VALUES(?,?,?)`,
			o.Currency, o.PayAmount, now+cooldownSec*1000); err != nil {
			tx.Rollback()
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO callbacks(order_id,event,next_at) VALUES(?, 'expired', ?)`, o.ID, now); err != nil {
			tx.Rollback()
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// CloseOrder 商户主动关单。
func (s *Store) CloseOrder(id string, cooldownSec int64) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.OrderByID(id)
	if err != nil {
		return nil, err
	}
	if o.Status != "pending" {
		return nil, ErrState
	}
	now := nowMs()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE orders SET status='closed' WHERE id=? AND status='pending'`, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO cooldown(currency,amount,until) VALUES(?,?,?)`,
		o.Currency, o.PayAmount, now+cooldownSec*1000); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	o.Status = "closed"
	return o, nil
}

func (s *Store) Consumed(binanceOrderID string) (bool, error) {
	var x string
	err := s.db.QueryRow(`SELECT order_id FROM consumed WHERE binance_order_id=?`, binanceOrderID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) DueCallbacks(limit int) ([]cbJob, error) {
	rows, err := s.db.Query(`SELECT id,order_id,event,attempt FROM callbacks WHERE done=0 AND next_at<=? ORDER BY id LIMIT ?`, nowMs(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cbJob
	for rows.Next() {
		var j cbJob
		if err := rows.Scan(&j.ID, &j.OrderID, &j.Event, &j.Attempt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) CallbackDone(id int64) error {
	_, err := s.db.Exec(`UPDATE callbacks SET done=1, last_err='' WHERE id=?`, id)
	return err
}

func (s *Store) CallbackRetry(id int64, attempt int, nextAt int64, lastErr string, giveUp bool) error {
	if giveUp {
		_, err := s.db.Exec(`UPDATE callbacks SET done=1, attempt=?, last_err=? WHERE id=?`, attempt, "GIVE_UP: "+lastErr, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE callbacks SET attempt=?, next_at=?, last_err=? WHERE id=?`, attempt, nextAt, lastErr, id)
	return err
}
