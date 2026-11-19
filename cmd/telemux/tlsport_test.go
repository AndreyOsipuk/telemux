package main

import "testing"

func TestTLSListenPort(t *testing.T) {
	if p, err := tlsListenPort("0.0.0.0:18443"); err != nil || p != 18443 {
		t.Fatalf("0.0.0.0:18443: %d %v", p, err)
	}
	for _, bad := range []string{"", "18443", "0.0.0.0:abc", "0.0.0.0:0", "0.0.0.0:70000"} {
		if _, err := tlsListenPort(bad); err == nil {
			t.Fatalf("%q: ждали ошибку", bad)
		}
	}
}
