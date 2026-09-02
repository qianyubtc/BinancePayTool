package main

import (
	"encoding/json"
	"os"
	"testing"
)

type vectorFile struct {
	Secret string `json:"secret"`
	Cases  []struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Nonce     string `json:"nonce"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		Body      string `json:"body"`
		Signature string `json:"signature"`
	} `json:"cases"`
}

func TestSignatureVectors(t *testing.T) {
	data, err := os.ReadFile("../docs/test_vectors.json")
	if err != nil {
		t.Fatalf("读取向量文件: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(data, &vf); err != nil {
		t.Fatalf("解析向量文件: %v", err)
	}
	if len(vf.Cases) == 0 {
		t.Fatal("向量为空")
	}
	for i, c := range vf.Cases {
		var got string
		switch c.Type {
		case "request":
			got = signRequest(vf.Secret, c.Timestamp, c.Nonce, c.Method, c.Path, []byte(c.Body))
		case "callback":
			got = signCallback(vf.Secret, c.Timestamp, c.Nonce, []byte(c.Body))
		default:
			t.Fatalf("未知类型 %s", c.Type)
		}
		if got != c.Signature {
			t.Errorf("case %d (%s): got %s want %s", i, c.Type, got, c.Signature)
		}
	}
}
