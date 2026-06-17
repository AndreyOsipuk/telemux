// Package selfupdate — самообновление бинаря telemux (как «обновить панель» в 3x-ui).
//
// Источник — GitHub Releases (артефакты goreleaser: сырые бинари
// telemux_<os>_<arch> + checksums.txt). Алгоритм: узнать последнюю версию →
// скачать бинарь под текущие GOOS/GOARCH → СВЕРИТЬ SHA256 по checksums.txt →
// атомарно подменить (backup .bak → rename) → вызывающий рестартит сервис →
// при сбое health-check откат на .bak. Подпись cosign — TODO.
package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Release — релиз с GitHub.
type Release struct {
	Version string  // tag_name, напр. v0.2.0
	Assets  []Asset
}

// Asset — артефакт релиза.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// AssetName возвращает имя бинаря-артефакта для ОС/арки (как у goreleaser).
func AssetName(goos, goarch string) string {
	return fmt.Sprintf("telemux_%s_%s", goos, goarch)
}

// PickAsset находит артефакт по имени.
func PickAsset(r Release, name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// ParseChecksums разбирает checksums.txt (строки "<sha256>  <имя файла>").
func ParseChecksums(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 {
			out[fields[len(fields)-1]] = strings.ToLower(fields[0])
		}
	}
	return out
}

// sha256hex считает SHA256 данных в hex.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DefaultAPIBase — база GitHub API (переопределяется в тестах / для зеркала).
const DefaultAPIBase = "https://api.github.com"

// FetchLatest запрашивает последний релиз через GitHub API.
func FetchLatest(ctx context.Context, hc *http.Client, apiBase, owner, repo string) (Release, error) {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", strings.TrimRight(apiBase, "/"), owner, repo)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("github releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}
	var raw struct {
		TagName string  `json:"tag_name"`
		Assets  []Asset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Release{}, fmt.Errorf("github releases: разбор: %w", err)
	}
	return Release{Version: raw.TagName, Assets: raw.Assets}, nil
}

func download(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d при скачивании %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// Options — параметры обновления.
type Options struct {
	Owner          string
	Repo           string
	CurrentVersion string // напр. v0.1.0 или dev
	GOOS           string
	GOARCH         string
	BinaryPath     string // путь к текущему бинарю (os.Executable())
	HTTPClient     *http.Client
	APIBase        string // пусто → DefaultAPIBase (GitHub); переопределяется для зеркала/тестов
}

// Update скачивает последнюю версию, верифицирует SHA256 и атомарно подменяет бинарь.
// Возвращает (старая, новая) версии. НЕ перезапускает сервис — это делает вызывающий.
// При несовпадении checksum НЕ трогает бинарь.
func Update(ctx context.Context, opts Options) (from, to string, err error) {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	rel, err := FetchLatest(ctx, hc, opts.APIBase, opts.Owner, opts.Repo)
	if err != nil {
		return opts.CurrentVersion, "", err
	}
	if rel.Version == opts.CurrentVersion {
		return opts.CurrentVersion, rel.Version, nil // уже последняя
	}

	wantName := AssetName(opts.GOOS, opts.GOARCH)
	binAsset, ok := PickAsset(rel, wantName)
	if !ok {
		return opts.CurrentVersion, rel.Version, fmt.Errorf("в релизе %s нет артефакта %s", rel.Version, wantName)
	}
	sumsAsset, ok := PickAsset(rel, "checksums.txt")
	if !ok {
		return opts.CurrentVersion, rel.Version, fmt.Errorf("в релизе %s нет checksums.txt", rel.Version)
	}

	sumsRaw, err := download(ctx, hc, sumsAsset.URL)
	if err != nil {
		return opts.CurrentVersion, rel.Version, fmt.Errorf("скачивание checksums: %w", err)
	}
	wantSum, ok := ParseChecksums(string(sumsRaw))[wantName]
	if !ok {
		return opts.CurrentVersion, rel.Version, fmt.Errorf("нет SHA256 для %s в checksums.txt", wantName)
	}

	binData, err := download(ctx, hc, binAsset.URL)
	if err != nil {
		return opts.CurrentVersion, rel.Version, fmt.Errorf("скачивание бинаря: %w", err)
	}
	if got := sha256hex(binData); got != wantSum {
		return opts.CurrentVersion, rel.Version, fmt.Errorf("checksum не совпал: ждали %s, получили %s — обновление отменено", wantSum, got)
	}

	if err := swapBinary(opts.BinaryPath, binData); err != nil {
		return opts.CurrentVersion, rel.Version, err
	}
	return opts.CurrentVersion, rel.Version, nil
}

// swapBinary атомарно подменяет бинарь: temp в той же директории → backup .bak → rename.
func swapBinary(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".telemux-new-*")
	if err != nil {
		return fmt.Errorf("temp-файл: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // если не переименовали
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("запись temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	// backup текущего (для отката)
	_ = os.Rename(path, path+".bak")
	if err := os.Rename(tmpName, path); err != nil {
		// попытка вернуть backup
		_ = os.Rename(path+".bak", path)
		return fmt.Errorf("подмена бинаря: %w", err)
	}
	return nil
}

// Rollback возвращает бинарь из .bak (для авто-отката при неуспешном health-check).
func Rollback(path string) error {
	bak := path + ".bak"
	if _, err := os.Stat(bak); err != nil {
		return fmt.Errorf("backup %s отсутствует: %w", bak, err)
	}
	return os.Rename(bak, path)
}
