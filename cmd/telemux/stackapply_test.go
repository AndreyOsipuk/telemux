package main

import "testing"

// Неверные параметры - код 2, до скачивания и замены дело не доходит.
func TestRunStackApply_BadArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--product", "xray"},
		{"--product", "telemt", "--bin", "/nonexistent/telemt", "--token-file", "/nonexistent/token"},
	} {
		if code := runStackApply(args); code != 2 {
			t.Fatalf("%v: ждали код 2, получили %d", args, code)
		}
	}
}
