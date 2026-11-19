package render

import (
	"strings"
	"testing"
)

func TestSecretDiff_Hunks(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	b := "1\n2\n3\n4\nX\n6\n7\n8\n9\n10\n11\n12\n13\n"
	want := `--- cur
+++ cand
@@ -2,7 +2,7 @@
 2
 3
 4
-5
+X
 6
 7
 8
@@ -10,3 +10,4 @@
 10
 11
 12
+13
`
	if got := SecretDiff(a, b, "cur", "cand"); got != want {
		t.Fatalf("diff:\n%s\nждали:\n%s", got, want)
	}
	if got := SecretDiff(a, a, "cur", "cand"); got != "" {
		t.Fatalf("одинаковые тексты: %q", got)
	}
	// Текущего нет - весь кандидат добавлен.
	if got := SecretDiff("", "a\nb\n", "cur", "cand"); got != "--- cur\n+++ cand\n@@ -0,0 +1,2 @@\n+a\n+b\n" {
		t.Fatalf("пустой текущий:\n%s", got)
	}
	if got := SecretDiff("a\nb\n", "", "cur", "cand"); got != "--- cur\n+++ cand\n@@ -1,2 +0,0 @@\n-a\n-b\n" {
		t.Fatalf("пустой кандидат:\n%s", got)
	}
}

// Секреты пользователей в diff не попадают; смена секрета видна по ключу.
func TestSecretDiff_Masks(t *testing.T) {
	cur := "debug = false\n\n[secrets]\n\"alice_1\" = \"ee0123\"\n\"bob_2\" = \"eeaaaa\"\n"
	cand := "debug = true\n\n[secrets]\n\"alice_1\" = \"ee0123\"\n\"bob_2\" = \"eebbbb\"\n\"carol_3\" = \"eecccc\"\n"
	got := SecretDiff(cur, cand, "cur", "cand")
	for _, leak := range []string{"ee0123", "eeaaaa", "eebbbb", "eecccc"} {
		if strings.Contains(got, leak) {
			t.Fatalf("секрет %s в diff:\n%s", leak, got)
		}
	}
	for _, want := range []string{"-debug = false", "+debug = true", " \"alice_1\" = <secret>", "-\"bob_2\" = <secret>",
		"+\"bob_2\" = <secret changed>", "+\"carol_3\" = <secret>"} {
		if !strings.Contains(got, want+"\n") {
			t.Fatalf("нет строки %q:\n%s", want, got)
		}
	}
	// [access.users] telemt тоже маскируется, обычные секции - нет.
	got = SecretDiff("[access.users]\nu = \"s1\"\n[server]\nport = 1\n", "[access.users]\nu = \"s2\"\n[server]\nport = 2\n", "a", "b")
	if strings.Contains(got, "s1") || strings.Contains(got, "s2") || !strings.Contains(got, "+port = 2") {
		t.Fatalf("telemt:\n%s", got)
	}
}

// Сильно разные длинные тексты не вешают сборку: замена целиком.
func TestSecretDiff_Large(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 5000; i++ {
		a.WriteString("a" + strings.Repeat("x", i%7) + "\n")
		b.WriteString("b" + strings.Repeat("y", i%5) + "\n")
	}
	got := SecretDiff(a.String(), b.String(), "a", "b")
	if strings.Count(got, "\n-") != 5000 || strings.Count(got, "\n+") != 5001 {
		t.Fatalf("строк удалено %d, добавлено %d", strings.Count(got, "\n-"), strings.Count(got, "\n+"))
	}
}

// Сценарий правки на случайных текстах: равные строки совпадают, из него
// собираются обе стороны.
func TestMyers_Random(t *testing.T) {
	seed := uint32(7)
	rnd := func(n uint32) int { seed = seed*1664525 + 1013904223; return int(seed>>16) % int(n) }
	for iter := 0; iter < 300; iter++ {
		gen := func() []string {
			out := make([]string, rnd(30))
			for i := range out {
				out[i] = string(rune('a' + rnd(4)))
			}
			return out
		}
		a, b := gen(), gen()
		var ra, rb []string
		for _, e := range myers(a, b) {
			switch e.op {
			case opEq:
				if a[e.a] != b[e.b] {
					t.Fatalf("равные строки не равны: %v %v", a, b)
				}
				ra, rb = append(ra, a[e.a]), append(rb, b[e.b])
			case opDel:
				ra = append(ra, a[e.a])
			case opIns:
				rb = append(rb, b[e.b])
			}
		}
		if strings.Join(ra, "") != strings.Join(a, "") || strings.Join(rb, "") != strings.Join(b, "") {
			t.Fatalf("сценарий не собирает стороны: %v → %v", a, b)
		}
	}
}
