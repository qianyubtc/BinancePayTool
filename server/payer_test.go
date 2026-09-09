package main

import (
	"encoding/json"
	"testing"
)

// 回调与查单必须同时给出兼容字段 payer_id 与稳定标识 counterparty_id / payer_binance_id。
func TestCallbackPayloadPayerFields(t *testing.T) {
	o := &Order{ID: "o1", AccountID: "default", MerchantOrderID: "M-1", Status: "paid", Currency: "USDT", BaseAmount: 1000000, PayAmount: 1000037, ActualAmount: 1000037,
		MatchedBy: "amount", BinanceOrderID: "452021922068888888", PayerID: "1160000000", PayerBinanceID: "1160000000", CounterpartyID: "1163000000"}
	b, err := callbackPayload(o, "paid")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["payer_id"] != "1160000000" || m["payer_binance_id"] != "1160000000" || m["counterparty_id"] != "1163000000" {
		t.Fatalf("payer fields missing: %v", m)
	}
}

func TestPayerOfAndIDStr(t *testing.T) {
	var tx payTxn
	tx.CounterpartyID = 1163000000
	if payerOf(tx) != "1163000000" || idStr(tx.PayerInfo.BinanceID) != "" || idStr(tx.CounterpartyID) != "1163000000" {
		t.Fatal("counterparty fallback / idStr")
	}
	tx.PayerInfo.BinanceID = 1160000000
	if payerOf(tx) != "1160000000" {
		t.Fatal("binanceId should win for payer_id")
	}
}
