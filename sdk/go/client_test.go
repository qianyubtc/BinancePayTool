package bpaygate

import (
	"encoding/json"
	"os"
	"testing"
)

func TestVectors(t *testing.T) {
	data, err := os.ReadFile("../../docs/test_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf struct {
		Secret string `json:"secret"`
		Cases  []struct {
			Type, Timestamp, Nonce, Method, Path, Body, Signature string
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &vf); err != nil {
		t.Fatal(err)
	}
	for i, c := range vf.Cases {
		var got string
		if c.Type == "request" {
			got = SignRequest(vf.Secret, c.Timestamp, c.Nonce, c.Method, c.Path, []byte(c.Body))
		} else {
			got = SignCallback(vf.Secret, c.Timestamp, c.Nonce, []byte(c.Body))
		}
		if got != c.Signature {
			t.Errorf("case %d: got %s want %s", i, got, c.Signature)
		}
	}
}
