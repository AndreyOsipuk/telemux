package render

import (
	"fmt"
	"regexp"
	"strings"
)

// Секции, значения которых - секреты пользователей: в diff вместо них <secret>.
var secretSections = map[string]bool{"secrets": true, "access.users": true}

var (
	sectionRe = regexp.MustCompile(`^\s*\[\s*([^\[\]]+?)\s*\]\s*(#.*)?$`)
	kvRe      = regexp.MustCompile(`^(\s*("[^"]*"|[^=\s]+)\s*=\s*)(.*)$`)
)

const (
	maskedSecret  = "<secret>"
	maskedChanged = "<secret changed>"
)

// secretValues - значения строк секретных секций по ключу.
func secretValues(lines []string) map[string]string {
	out := map[string]string{}
	section := ""
	for _, l := range lines {
		if m := sectionRe.FindStringSubmatch(l); m != nil {
			section = m[1]
			continue
		}
		if !secretSections[section] {
			continue
		}
		if m := kvRe.FindStringSubmatch(l); m != nil {
			out[section+"\x00"+m[2]] = m[3]
		}
	}
	return out
}

// maskSecrets заменяет значения строк секретных секций на <secret>; если у того же
// ключа в other значение другое - на <secret changed>, чтобы смена ключа была видна
// в diff без самого ключа.
func maskSecrets(lines []string, other map[string]string) []string {
	out := make([]string, len(lines))
	section := ""
	for i, l := range lines {
		out[i] = l
		if m := sectionRe.FindStringSubmatch(l); m != nil {
			section = m[1]
			continue
		}
		if !secretSections[section] {
			continue
		}
		if m := kvRe.FindStringSubmatch(l); m != nil {
			mask := maskedSecret
			if v, ok := other[section+"\x00"+m[2]]; ok && v != m[3] {
				mask = maskedChanged
			}
			out[i] = m[1] + mask
		}
	}
	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// SecretDiff - unified diff текущего содержимого (current) и кандидата с
// замаскированными секретами пользователей. Одинаковые тексты - пустая строка.
func SecretDiff(current, candidate, nameCurrent, nameCandidate string) string {
	if current == candidate {
		return ""
	}
	a, b := splitLines(current), splitLines(candidate)
	// У текущего - просто <secret>, у кандидата сменённый ключ - <secret changed>:
	// так строка попадает в diff парой -/+.
	return unifiedDiff(maskSecrets(a, nil), maskSecrets(b, secretValues(a)), nameCurrent, nameCandidate, 3)
}

// Операции правки.
const (
	opEq  = ' '
	opDel = '-'
	opIns = '+'
)

type edit struct {
	op   byte
	a, b int // номера строк в a и b (для opIns a - позиция вставки)
}

// maxDiffD - предел числа правок для алгоритма Майерса; больше - текст заменён целиком.
const maxDiffD = 4000

// myers - кратчайший сценарий правки a → b (O((N+M)·D)).
func myers(a, b []string) []edit {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return nil
	}
	limit := max
	if limit > maxDiffD {
		limit = maxDiffD
	}
	off := limit + 1
	v := make([]int, 2*limit+3)
	var trace [][]int
	found := false
	for d := 0; d <= limit && !found; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	if !found {
		// Слишком разные тексты: удалить всё и вставить всё.
		var out []edit
		for i := range a {
			out = append(out, edit{opDel, i, 0})
		}
		for j := range b {
			out = append(out, edit{opIns, n, j})
		}
		return out
	}
	// Обратный проход по сохранённым состояниям.
	var rev []edit
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		vd := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && vd[off+k-1] < vd[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := vd[off+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, edit{opEq, x, y})
		}
		if d > 0 {
			if x == prevX {
				y--
				rev = append(rev, edit{opIns, x, y})
			} else {
				x--
				rev = append(rev, edit{opDel, x, y})
			}
		}
	}
	out := make([]edit, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out
}

// unifiedDiff - diff в формате diff -u с ctx строками контекста.
func unifiedDiff(a, b []string, nameA, nameB string, ctx int) string {
	edits := myers(a, b)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", nameA, nameB)
	i := 0
	for i < len(edits) {
		// Следующая правка.
		for i < len(edits) && edits[i].op == opEq {
			i++
		}
		if i >= len(edits) {
			break
		}
		start := i - ctx
		if start < 0 {
			start = 0
		}
		// Конец ханка: правки, между которыми не больше 2*ctx равных строк.
		end := i
		for j := i; j < len(edits); j++ {
			if edits[j].op != opEq {
				end = j
				continue
			}
			if j-end > 2*ctx {
				break
			}
		}
		stop := end + ctx + 1
		if stop > len(edits) {
			stop = len(edits)
		}
		var aStart, bStart, aLen, bLen int
		aStart, bStart = -1, -1
		for _, e := range edits[start:stop] {
			switch e.op {
			case opEq:
				if aStart < 0 {
					aStart = e.a
				}
				if bStart < 0 {
					bStart = e.b
				}
				aLen++
				bLen++
			case opDel:
				if aStart < 0 {
					aStart = e.a
				}
				aLen++
			case opIns:
				if bStart < 0 {
					bStart = e.b
				}
				bLen++
			}
		}
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", hunkRange(aStart, aLen, edits[start:stop], true), hunkRange(bStart, bLen, edits[start:stop], false))
		for _, e := range edits[start:stop] {
			switch e.op {
			case opEq:
				sb.WriteString(" " + a[e.a] + "\n")
			case opDel:
				sb.WriteString("-" + a[e.a] + "\n")
			case opIns:
				sb.WriteString("+" + b[e.b] + "\n")
			}
		}
		i = stop
	}
	return sb.String()
}

// hunkRange - «начало,длина» ханка (с единицы; пустая сторона - позиция перед ней).
func hunkRange(start, n int, es []edit, sideA bool) string {
	if start < 0 {
		// На этой стороне нет строк: позиция - строка перед вставкой/удалением.
		if sideA {
			start = es[0].a
		} else {
			start = es[0].b
		}
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}
