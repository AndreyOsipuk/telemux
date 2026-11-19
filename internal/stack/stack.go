// Package stack - общий для мастера и агента формат ресурса stack (ADR 0008): какие версии
// бинарей telemt и mtg-multi должны стоять на машине, и правила сравнения версий.
package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Resource - имя ресурса в desired_config.
const Resource = "stack"

// Продукты, которые обновляет ресурс.
const (
	ProductTelemt = "telemt"
	ProductMtg    = "mtg"
)

// Products - все продукты в порядке вывода.
var Products = []string{ProductTelemt, ProductMtg}

// ValidProduct - известен ли продукт.
func ValidProduct(p string) bool { return p == ProductTelemt || p == ProductMtg }

// Binary - бинарь продукта в ресурсе: версия релиза, SHA256 и размер самого бинаря (не архива).
// Агент берёт его у мастера: GET /v1/stack/blobs/{SHA256}?target=<машина>.
type Binary struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// Content - содержимое ресурса stack: продукт -> бинарь. Продукта нет - его не трогают.
type Content map[string]Binary

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 - строка из 64 строчных hex-символов.
func ValidSHA256(s string) bool { return sha256Re.MatchString(s) }

// Parse разбирает и проверяет содержимое ресурса.
func Parse(b []byte) (Content, error) {
	var c Content
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("stack: не JSON: %w", err)
	}
	for p, bin := range c {
		if !ValidProduct(p) {
			return nil, fmt.Errorf("stack: неизвестный продукт %q", p)
		}
		if bin.Version == "" || !ValidSHA256(bin.SHA256) || bin.Size <= 0 {
			return nil, fmt.Errorf("stack: %s: нужны version, sha256 (64 hex) и size > 0", p)
		}
	}
	return c, nil
}

// Marshal - детерминированный JSON (ключи map encoding/json сортирует сам).
func (c Content) Marshal() ([]byte, error) { return json.Marshal(c) }

// Version - разобранная версия: ядро X.Y.Z и суффикс после него.
type Version struct {
	Major, Minor, Patch int
	Suffix              string // "-webalias", "-15-g039ef04"; пусто - чистый релиз
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(.*)$`)

// ErrBadVersion - версию не удалось разобрать.
var ErrBadVersion = errors.New("версия не разобрана")

// ParseVersion разбирает «3.5.17», «v1.12.0-15-g039ef04», «1.15.0-webalias». Пробелы по
// краям отбрасываются. Нет ядра X.Y.Z - ErrBadVersion.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, ErrBadVersion
	}
	var v Version
	var err error
	if v.Major, err = strconv.Atoi(m[1]); err != nil {
		return Version{}, ErrBadVersion
	}
	if v.Minor, err = strconv.Atoi(m[2]); err != nil {
		return Version{}, ErrBadVersion
	}
	if v.Patch, err = strconv.Atoi(m[3]); err != nil {
		return Version{}, ErrBadVersion
	}
	v.Suffix = m[4]
	return v, nil
}

// Core - «X.Y.Z».
func (v Version) Core() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// CompareCore сравнивает ядра: -1, 0, 1.
func CompareCore(a, b Version) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Assessment - оценка версии на машине против последнего релиза.
type Assessment struct {
	CustomBuild     bool // суффикс после ядра или версия не разобрана
	UpdateAvailable bool // ядро релиза строго больше ядра на машине
}

// Assess сравнивает версию на машине с последним релизом. Пустая текущая версия - не своя
// сборка и без обновления (нечего сравнивать). Неразобранный релиз - обновления нет.
func Assess(current, latest string) Assessment {
	if strings.TrimSpace(current) == "" {
		return Assessment{}
	}
	cur, err := ParseVersion(current)
	if err != nil {
		return Assessment{CustomBuild: true}
	}
	a := Assessment{CustomBuild: cur.Suffix != ""}
	if lat, err := ParseVersion(latest); err == nil {
		a.UpdateAvailable = CompareCore(lat, cur) > 0
	}
	return a
}

// SameVersion - версия на машине совпадает с версией релиза: то же ядро и тот же суффикс
// (ведущая v не важна). Агент по ней решает, что обновление уже стоит.
func SameVersion(current, release string) bool {
	c, err1 := ParseVersion(current)
	r, err2 := ParseVersion(release)
	if err1 != nil || err2 != nil {
		return strings.TrimSpace(current) == strings.TrimSpace(release)
	}
	return CompareCore(c, r) == 0 && c.Suffix == r.Suffix
}
