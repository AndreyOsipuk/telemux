package telemtcfg

// Сборка основного telemt.toml (ресурс telemt.config) из трёх частей с разными
// хозяевами - так два ресурса не перетирают один файл:
//
//   - база - текст ресурса telemt.config, его готовит бот (generateMainConfig):
//     общая статика, announce_ip из базы, include на users.toml;
//   - машинная часть - файл на самой машине (по умолчанию telemt.local.toml рядом
//     с telemt.toml), его ведёт человек: WEB-режим ([web], [[web.vhosts]]),
//     локальный listener, mask_port, свои каталоги - всё, что на релеях правится
//     руками при переездах. Машинная часть сильнее базы;
//   - WEB-профили [[web.vhosts.profiles]] - их хозяин ресурс telemt.users: агент
//     пересобирает их из списка пользователей (webprofiles.go). В базе и в
//     машинной части профилей быть не должно.
//
// Правила слияния машинной части с базой:
//   - [таблица]: ключи машинной части заменяют одноимённые ключи базы на их месте,
//     новых ключей - дописываются в конец таблицы; таблицы, которой в базе нет, -
//     дописывается в конец файла;
//   - [[массив]]: группа целиком (все элементы и их подтаблицы) заменяет группу
//     базы на её месте; если в базе такой группы нет - дописывается в конец.
//
// Разбор построчный, без TOML-библиотеки: важно сохранить базу байт в байт там,
// где машина её не трогает. Значение telemt проверяет сам (config_strict).

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// webProfilesTable - имя таблицы WEB-профилей.
const webProfilesTable = "web.vhosts.profiles"

// tomlSection - секция файла: заголовок и строки тела до следующего заголовка.
type tomlSection struct {
	header string // строка заголовка как в файле; "" - корень до первого заголовка
	name   string // путь таблицы: server.listeners
	array  bool   // [[...]]
	lines  []string
}

// tomlEntry - ключ в теле секции: строки [start, end), lead - начало комментариев над ним.
type tomlEntry struct {
	key              string
	lead, start, end int
}

type scanState struct {
	depth  int    // открытые [ и {
	triple string // открытая многострочная строка: """ или '''
}

func (s scanState) clean() bool { return s.depth == 0 && s.triple == "" }

// scanLine проходит строку значения: учитывает строки, скобки и комментарий.
// Возвращает состояние после строки и начало комментария (-1 - нет).
func scanLine(line string, st scanState) (scanState, int, error) {
	for i := 0; i < len(line); {
		if st.triple != "" {
			j := strings.Index(line[i:], st.triple)
			if j < 0 {
				return st, -1, nil
			}
			i += j + 3
			st.triple = ""
			continue
		}
		switch c := line[i]; {
		case c == '#':
			return st, i, nil
		case strings.HasPrefix(line[i:], `"""`), strings.HasPrefix(line[i:], `'''`):
			st.triple = line[i : i+3]
			i += 3
		case c == '"':
			j := i + 1
			for j < len(line) && line[j] != '"' {
				if line[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(line) {
				return st, -1, errors.New("незакрытая строка")
			}
			i = j + 1
		case c == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return st, -1, errors.New("незакрытая строка")
			}
			i += j + 2
		case c == '[' || c == '{':
			st.depth++
			i++
		case c == ']' || c == '}':
			st.depth--
			if st.depth < 0 {
				return st, -1, errors.New("лишняя закрывающая скобка")
			}
			i++
		default:
			i++
		}
	}
	return st, -1, nil
}

var (
	arrayHeaderRe = regexp.MustCompile(`^\s*\[\[\s*([^\[\]#]+?)\s*\]\]\s*(#.*)?$`)
	tableHeaderRe = regexp.MustCompile(`^\s*\[\s*([^\[\]#]+?)\s*\]\s*(#.*)?$`)
	tomlKeyRe     = regexp.MustCompile(`^\s*((?:[A-Za-z0-9_\-]+|"[^"]*"|'[^']*')(?:\s*\.\s*(?:[A-Za-z0-9_\-]+|"[^"]*"|'[^']*'))*)\s*=`)
)

func normName(raw string) string {
	parts := strings.Split(raw, ".")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ".")
}

// splitSections режет текст на секции. Корень (до первого заголовка) есть всегда.
func splitSections(text string) ([]tomlSection, error) {
	secs := []tomlSection{{}}
	var st scanState
	for n, line := range strings.Split(text, "\n") {
		if st.clean() && strings.HasPrefix(strings.TrimSpace(line), "[") {
			if m := arrayHeaderRe.FindStringSubmatch(line); m != nil {
				secs = append(secs, tomlSection{header: line, name: normName(m[1]), array: true})
				continue
			}
			if m := tableHeaderRe.FindStringSubmatch(line); m != nil {
				secs = append(secs, tomlSection{header: line, name: normName(m[1])})
				continue
			}
			return nil, fmt.Errorf("строка %d: битый заголовок таблицы %q", n+1, strings.TrimSpace(line))
		}
		var err error
		if st, _, err = scanLine(line, st); err != nil {
			return nil, fmt.Errorf("строка %d: %v", n+1, err)
		}
		last := &secs[len(secs)-1]
		last.lines = append(last.lines, line)
	}
	if !st.clean() {
		return nil, errors.New("незакрытое значение в конце файла (массив, таблица или многострочная строка)")
	}
	for _, s := range secs {
		if _, err := s.entries(); err != nil {
			return nil, err
		}
	}
	return secs, nil
}

func joinSections(secs []tomlSection) string {
	var out []string
	for _, s := range secs {
		if s.header != "" {
			out = append(out, s.header)
		}
		out = append(out, s.lines...)
	}
	return strings.Join(out, "\n")
}

func (s tomlSection) title() string {
	switch {
	case s.header == "":
		return "(корень)"
	case s.array:
		return "[[" + s.name + "]]"
	default:
		return "[" + s.name + "]"
	}
}

// entries - ключи тела секции.
func (s tomlSection) entries() ([]tomlEntry, error) {
	var out []tomlEntry
	lead := -1
	for i := 0; i < len(s.lines); i++ {
		t := strings.TrimSpace(s.lines[i])
		if t == "" {
			lead = -1
			continue
		}
		if strings.HasPrefix(t, "#") {
			if lead < 0 {
				lead = i
			}
			continue
		}
		m := tomlKeyRe.FindStringSubmatch(s.lines[i])
		if m == nil {
			return nil, fmt.Errorf("%s: строка %q не похожа на «ключ = значение»", s.title(), t)
		}
		e := tomlEntry{key: normName(m[1]), lead: i, start: i}
		if lead >= 0 {
			e.lead = lead
		}
		st, _, err := scanLine(s.lines[i][len(m[0]):], scanState{})
		if err != nil {
			return nil, fmt.Errorf("%s, ключ %s: %v", s.title(), e.key, err)
		}
		for !st.clean() && i+1 < len(s.lines) {
			i++
			if st, _, err = scanLine(s.lines[i], st); err != nil {
				return nil, fmt.Errorf("%s, ключ %s: %v", s.title(), e.key, err)
			}
		}
		e.end = i + 1
		out = append(out, e)
		lead = -1
	}
	return out, nil
}

// entryValue - значение ключа без комментариев и лишних пробелов (для сравнения).
func (s tomlSection) entryValue(e tomlEntry) string {
	var parts []string
	var st scanState
	for i := e.start; i < e.end; i++ {
		line := s.lines[i]
		next, c, _ := scanLine(line, st)
		if c >= 0 {
			line = line[:c]
		}
		st = next
		parts = append(parts, strings.Join(strings.Fields(line), " "))
	}
	return strings.Join(parts, " ")
}

func findEntry(es []tomlEntry, key string) (tomlEntry, bool) {
	for _, e := range es {
		if e.key == key {
			return e, true
		}
	}
	return tomlEntry{}, false
}

// bodyEnd - конец значимой части тела (без хвостовых пустых строк).
func bodyEnd(lines []string) int {
	n := len(lines)
	for n > 0 && strings.TrimSpace(lines[n-1]) == "" {
		n--
	}
	return n
}

// mergeKeys переносит ключи ov в base: заменяет на месте или дописывает в конец.
func mergeKeys(base *tomlSection, ov tomlSection) {
	oes, _ := ov.entries()
	for _, o := range oes {
		chunk := append([]string(nil), ov.lines[o.lead:o.end]...)
		bes, _ := base.entries()
		var next []string
		if b, ok := findEntry(bes, o.key); ok {
			next = append(next, base.lines[:b.start]...)
			next = append(next, chunk...)
			next = append(next, base.lines[b.end:]...)
		} else {
			at := bodyEnd(base.lines)
			next = append(next, base.lines[:at]...)
			next = append(next, chunk...)
			next = append(next, base.lines[at:]...)
		}
		base.lines = next
	}
}

// item - секция-таблица или группа массива (элементы [[N]] с их подтаблицами).
type item struct {
	name  string
	array bool
	secs  []tomlSection
}

func groupItems(secs []tomlSection) []item {
	var out []item
	for i := 0; i < len(secs); {
		s := secs[i]
		if !s.array {
			out = append(out, item{name: s.name, secs: []tomlSection{s}})
			i++
			continue
		}
		it := item{name: s.name, array: true}
		for i < len(secs) && ((secs[i].array && secs[i].name == s.name) || strings.HasPrefix(secs[i].name, s.name+".")) {
			it.secs = append(it.secs, secs[i])
			i++
		}
		out = append(out, it)
	}
	return out
}

func trimTrailingBlank(secs []tomlSection) []tomlSection {
	out := append([]tomlSection(nil), secs...)
	last := &out[len(out)-1]
	last.lines = append(append([]string(nil), last.lines[:bodyEnd(last.lines)]...), "")
	return out
}

func hasTable(secs []tomlSection, name string) bool {
	for _, s := range secs {
		if s.name == name && s.header != "" {
			return true
		}
	}
	return false
}

// MergeOverlay накладывает машинную часть на базу (правила - в шапке файла).
// Пустая машинная часть возвращает базу байт в байт.
func MergeOverlay(base, overlay string) (string, error) {
	B, err := splitSections(base)
	if err != nil {
		return "", fmt.Errorf("telemt.config: %w", err)
	}
	O, err := splitSections(overlay)
	if err != nil {
		return "", fmt.Errorf("машинная часть: %w", err)
	}
	var appendix []tomlSection
	for _, it := range groupItems(O) {
		if !it.array {
			s := it.secs[0]
			if s.header == "" {
				mergeKeys(&B[0], s)
				continue
			}
			found := false
			for j := range B {
				if !B[j].array && B[j].header != "" && B[j].name == s.name {
					mergeKeys(&B[j], s)
					found = true
					break
				}
			}
			if !found {
				appendix = append(appendix, s)
			}
			continue
		}
		group := trimTrailingBlank(it.secs)
		var next []tomlSection
		at := -1
		for j := 0; j < len(B); {
			if B[j].array && B[j].name == it.name {
				if at < 0 {
					at = len(next)
				}
				for j < len(B) && ((B[j].array && B[j].name == it.name) || strings.HasPrefix(B[j].name, it.name+".")) {
					j++
				}
				continue
			}
			next = append(next, B[j])
			j++
		}
		if at < 0 {
			appendix = append(appendix, it.secs...)
			continue
		}
		B = append(next[:at], append(group, next[at:]...)...)
	}
	out := joinSections(B)
	if len(appendix) > 0 {
		var tail []string
		for _, s := range appendix {
			if s.header != "" {
				tail = append(tail, s.header)
			}
			tail = append(tail, s.lines[:bodyEnd(s.lines)]...)
			tail = append(tail, "")
		}
		out = strings.TrimRight(out, " \t\r\n") + "\n\n" + strings.TrimRight(strings.Join(tail, "\n"), "\n") + "\n"
	}
	return out, nil
}

func hasProfiles(secs []tomlSection) bool {
	return hasTable(secs, webProfilesTable)
}

// ComposeMainConfig собирает telemt.toml: база + машинная часть + WEB-профили
// под список пользователей с параметрами профилей по умолчанию.
func ComposeMainConfig(base, overlay string, users []string) (string, error) {
	return ComposeMainConfigFrom(base, overlay, "", users)
}

// ComposeMainConfigFrom - то же, но параметры профилей (secret_mode, лимиты) берутся из
// профилей current - telemt.toml на машине: они машинные, как и у telemt.users.
func ComposeMainConfigFrom(base, overlay, current string, users []string) (string, error) {
	B, err := splitSections(base)
	if err != nil {
		return "", fmt.Errorf("telemt.config: %w", err)
	}
	O, err := splitSections(overlay)
	if err != nil {
		return "", fmt.Errorf("машинная часть: %w", err)
	}
	if hasProfiles(B) {
		return "", errors.New("в telemt.config есть [[web.vhosts.profiles]] - профили собирает агент из telemt.users")
	}
	if hasProfiles(O) {
		return "", errors.New("в машинной части есть [[web.vhosts.profiles]] - профили собирает агент из telemt.users")
	}
	merged, err := MergeOverlay(base, overlay)
	if err != nil {
		return "", err
	}
	// Образец профиля машины кладётся в конец: ApplyWebProfiles прочитает из него
	// шаблон и заменит его профилями списка - текст тот же, что без образца.
	if tpl := firstProfile(current); tpl != "" && len(users) > 0 && webModeRe.MatchString(merged) {
		merged = strings.TrimRight(merged, " \t\r\n") + "\n\n" + tpl + "\n"
	}
	out, wp := ApplyWebProfiles(merged, users)
	if wp.Skipped == SkipOverLimit {
		return "", fmt.Errorf("WEB-профилей нужно %d, больше max_profiles - telemt такой конфиг не примет", len(users))
	}
	return out, nil
}

// firstProfile - первый блок [[web.vhosts.profiles]] конфига без хвостовых пустых строк.
func firstProfile(config string) string {
	secs, err := splitSections(config)
	if err != nil {
		return ""
	}
	for _, s := range secs {
		if s.name == webProfilesTable && s.array {
			return strings.Join(append([]string{s.header}, s.lines[:bodyEnd(s.lines)]...), "\n")
		}
	}
	return ""
}

// ValidateMainConfig - проверка текста telemt.config при публикации: разбирается,
// без WEB-профилей и с include на users.toml.
func ValidateMainConfig(text string) error {
	secs, err := splitSections(text)
	if err != nil {
		return err
	}
	if hasProfiles(secs) {
		return errors.New("в telemt.config не должно быть [[web.vhosts.profiles]] - профили собирает агент из telemt.users")
	}
	if len(includes(secs)) == 0 {
		return errors.New("в telemt.config нет include на users.toml - пользователи пропали бы с машины")
	}
	return nil
}

var quotedRe = regexp.MustCompile(`^"([^"]*)"|^'([^']*)'`)

// includes - значения ключей include во всех секциях.
func includes(secs []tomlSection) []string {
	var out []string
	for _, s := range secs {
		es, _ := s.entries()
		for _, e := range es {
			if strings.Trim(e.key, `"'`) != "include" {
				continue
			}
			v := s.entryValue(e)
			v = strings.TrimSpace(v[strings.Index(v, "=")+1:])
			if m := quotedRe.FindStringSubmatch(v); m != nil {
				out = append(out, m[1]+m[2])
			}
		}
	}
	return out
}

func entryValueOf(secs []tomlSection, table, key string) (string, bool) {
	for _, s := range secs {
		if s.name != table || s.header == "" {
			continue
		}
		es, _ := s.entries()
		if e, ok := findEntry(es, key); ok {
			v := s.entryValue(e)
			return strings.TrimSpace(v[strings.Index(v, "=")+1:]), true
		}
	}
	return "", false
}

var webTransportRe = regexp.MustCompile(`(?m)^\s*transport\s*=\s*"web"\s*$`)

// IsWebMode - на машине включён WEB-режим telemt.
func IsWebMode(config string) bool {
	if webTransportRe.MatchString(config) {
		return true
	}
	secs, err := splitSections(config)
	if err != nil {
		return strings.Contains(config, "[web]")
	}
	for _, s := range secs {
		if s.header != "" && (s.name == "web" || strings.HasPrefix(s.name, "web.")) {
			return true
		}
	}
	return false
}

// GuardMainConfig - последние проверки собранного конфига перед записью на машину:
// не теряется WEB-режим (инцидент с эталоном на WEB-релее: telemt в цикле
// «Address in use»), include ведёт на users.toml агента, API telemt не выключен
// (без него агенту нечем подтвердить применение).
func GuardMainConfig(current, next, usersFile string) error {
	if IsWebMode(current) && !IsWebMode(next) {
		return errors.New("на машине WEB-режим, а в собранном конфиге его нет - перенесите [web], [[web.vhosts]] и listener в машинную часть (telemt.local.toml)")
	}
	secs, err := splitSections(next)
	if err != nil {
		return err
	}
	vhosts := 0
	for _, s := range secs {
		if s.array && s.name == "web.vhosts" {
			vhosts++
		}
	}
	if vhosts > 1 {
		return fmt.Errorf("в собранном конфиге %d [[web.vhosts]] - профили собираются только под одним vhost, такой конфиг агент не ведёт", vhosts)
	}
	if usersFile != "" {
		inc := includes(secs)
		found := false
		for _, p := range inc {
			if p == usersFile {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("include в собранном конфиге (%v) не ведёт на users.toml агента %s - пользователи пропали бы", inc, usersFile)
		}
	}
	if v, ok := entryValueOf(secs, "server.api", "enabled"); ok && v == "false" {
		return errors.New("в собранном конфиге выключен API telemt ([server.api] enabled = false) - агенту нечем проверить применение")
	}
	return nil
}

// itemKey - ключ сравнения: таблица или группа массива.
func (it item) key() string {
	if it.array {
		return "[[" + it.name + "]]"
	}
	if it.secs[0].header == "" {
		return "(корень)"
	}
	return "[" + it.name + "]"
}

// itemText - содержимое без комментариев и форматирования.
func (it item) itemText() string {
	var parts []string
	for _, s := range it.secs {
		parts = append(parts, s.title())
		es, _ := s.entries()
		for _, e := range es {
			parts = append(parts, s.entryValue(e))
		}
	}
	return strings.Join(parts, "\n")
}

func indexItems(items []item) map[string]item {
	m := make(map[string]item, len(items))
	for _, it := range items {
		m[it.key()] = it
	}
	return m
}

// MainConfigDiff - чем have отличается от want по секциям ("" - ничем).
func MainConfigDiff(want, have string) string {
	if want == have {
		return ""
	}
	W, err := splitSections(want)
	if err != nil {
		return "желаемый конфиг не разбирается: " + err.Error()
	}
	H, err := splitSections(have)
	if err != nil {
		return "конфиг на машине не разбирается: " + err.Error()
	}
	wi, hi := indexItems(groupItems(W)), indexItems(groupItems(H))
	var differ, missing, extra []string
	for k, w := range wi {
		h, ok := hi[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case h.itemText() != w.itemText():
			differ = append(differ, k)
		}
	}
	for k := range hi {
		if _, ok := wi[k]; !ok {
			extra = append(extra, k)
		}
	}
	var parts []string
	add := func(title string, keys []string) {
		if len(keys) > 0 {
			sort.Strings(keys)
			parts = append(parts, title+": "+strings.Join(keys, ", "))
		}
	}
	add("отличаются", differ)
	add("нет на машине", missing)
	add("лишние на машине", extra)
	if len(parts) == 0 {
		return "отличаются только комментарии или форматирование"
	}
	return strings.Join(parts, "; ")
}

func sectionText(s tomlSection, lines []string) []string {
	var out []string
	if s.header != "" {
		out = append(out, s.header)
	}
	return append(out, lines...)
}

// DeriveOverlay выводит машинную часть из конфига на машине: всё, чем он
// отличается от базы, кроме WEB-профилей. warnings - отличия, которые машинная
// часть выразить не умеет (ключ или секция базы удалены на машине руками).
// Нужна один раз при переводе машины на telemt.config.
func DeriveOverlay(base, current string) (string, []string) {
	B, err := splitSections(base)
	if err != nil {
		return "", []string{"telemt.config не разбирается: " + err.Error()}
	}
	C, err := splitSections(current)
	if err != nil {
		return "", []string{"конфиг на машине не разбирается: " + err.Error()}
	}
	var noProfiles []tomlSection
	for _, s := range C {
		if s.name != webProfilesTable {
			noProfiles = append(noProfiles, s)
		}
	}
	bItems := groupItems(B)
	bi := indexItems(bItems)
	ci := indexItems(groupItems(noProfiles))
	var blocks [][]string
	var warns []string
	for _, it := range groupItems(noProfiles) {
		b, ok := bi[it.key()]
		if it.array {
			if !ok || b.itemText() != it.itemText() {
				var lines []string
				for _, s := range trimTrailingBlank(it.secs) {
					lines = append(lines, sectionText(s, s.lines)...)
				}
				blocks = append(blocks, lines)
			}
			continue
		}
		s := it.secs[0]
		if !ok {
			blocks = append(blocks, sectionText(s, s.lines[:bodyEnd(s.lines)]))
			continue
		}
		bs := b.secs[0]
		bes, _ := bs.entries()
		ces, _ := s.entries()
		var lines []string
		for _, e := range ces {
			be, found := findEntry(bes, e.key)
			if !found || bs.entryValue(be) != s.entryValue(e) {
				lines = append(lines, s.lines[e.lead:e.end]...)
			}
		}
		for _, e := range bes {
			if _, found := findEntry(ces, e.key); !found {
				warns = append(warns, fmt.Sprintf("в %s на машине нет ключа %s - машинная часть не умеет удалять, ключ придёт из telemt.config", s.title(), e.key))
			}
		}
		if len(lines) > 0 {
			blocks = append(blocks, sectionText(s, lines))
		}
	}
	for _, b := range bItems {
		if _, ok := ci[b.key()]; !ok {
			warns = append(warns, fmt.Sprintf("секции %s на машине нет - машинная часть не умеет удалять, секция придёт из telemt.config", b.key()))
		}
	}
	var out []string
	for _, b := range blocks {
		out = append(out, strings.TrimRight(strings.Join(b, "\n"), "\n"))
	}
	if len(out) == 0 {
		return "", warns
	}
	return strings.Join(out, "\n\n") + "\n", warns
}
