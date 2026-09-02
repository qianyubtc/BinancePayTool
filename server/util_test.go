package main

import "testing"

func TestParseFmtAmount(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"9500", 950000000000, false},
		{"0.1234", 12340000, false},
		{"10.0037", 1000370000, false},
		{"0.00000001", 1, false},
		{"-1", -100000000, false},
		{"1.123456789", 0, true},
		{"abc", 0, true},
		{"", 0, true},
		{"1e5", 0, true},
	}
	for _, c := range cases {
		got, err := parseAmount(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseAmount(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseAmount(%q)=%d,%v want %d", c.in, got, err, c.want)
		}
	}
	for _, s := range []string{"9500", "0.1234", "10.0037", "0.00000001", "1"} {
		v, _ := parseAmount(s)
		if fmtAmount(v) != s {
			t.Errorf("roundtrip %q → %q", s, fmtAmount(v))
		}
	}
	if fmtAmount(1000370000) != "10.0037" {
		t.Errorf("fmtAmount 尾零处理错误: %s", fmtAmount(1000370000))
	}
	if d := decimalsOf(1000370000); d != 4 {
		t.Errorf("decimalsOf=%d want 4", d)
	}
	if d := decimalsOf(950000000000); d != 0 {
		t.Errorf("decimalsOf=%d want 0", d)
	}
}
