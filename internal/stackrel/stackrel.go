// Package stackrel - релизы telemt и mtg-multi на GitHub для обновления машин (ADR 0008):
// последний релиз, релиз по версии, скачивание архива с проверкой SHA256 по контрольной
// сумме релиза и извлечение бинаря. Плюс кэш бинарей на мастере (cache.go).
package stackrel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/selfupdate"
	"github.com/AndreyOsipuk/telemux/internal/stack"
)

// Репозитории по умолчанию.
const (
	DefaultTelemtRepo = "telemt/telemt"
	DefaultMtgRepo    = "dolonet/mtg-multi"
	// DefaultAPIBase - база GitHub API.
	DefaultAPIBase = selfupdate.DefaultAPIBase
)

// Пределы размеров (переменные - для тестов).
var (
	// MaxArchive - архив релиза.
	MaxArchive int64 = 100 << 20
	// MaxBinary - бинарь в архиве после распаковки.
	MaxBinary int64 = 256 << 20
	// maxSmall - ответ API и файл контрольных сумм.
	maxSmall int64 = 4 << 20
)

// Таймауты: запрос к API и скачивание архива.
const (
	apiTimeout      = 30 * time.Second
	downloadTimeout = 5 * time.Minute
)

var (
	// ErrReleaseNotFound - релиза с такой версией (тегом) нет.
	ErrReleaseNotFound = errors.New("релиз не найден")
	// ErrUnsupportedArch - архитектура машины не поддерживается.
	ErrUnsupportedArch = errors.New("архитектура не поддерживается")
	// ErrAssetMissing - в релизе нет архива или файла контрольных сумм под архитектуру.
	ErrAssetMissing = errors.New("в релизе нет файла")
	// ErrBadReleaseVersion - версия из тега релиза не разбирается (нет ядра X.Y.Z).
	ErrBadReleaseVersion = errors.New("версия релиза не разобрана")
)

// Asset - файл релиза.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release - релиз продукта.
type Release struct {
	Product     string
	Repo        string
	Tag         string    // тег как в GitHub: 3.5.17, v1.15.0
	Version     string    // тег без ведущей v
	PublishedAt time.Time // published_at релиза
	Assets      []Asset
}

// Client - клиент релизов GitHub.
type Client struct {
	HTTP    *http.Client      // nil - http.DefaultClient (таймауты - через контекст)
	APIBase string            // пусто - DefaultAPIBase
	Token   string            // необязательный токен GitHub (только для запросов к API)
	Repos   map[string]string // продукт -> owner/repo; нет - репозиторий по умолчанию
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/([A-Za-z0-9_.-]+)$`)

// ValidRepo - строка вида owner/repo (repo не «.» и не «..»).
func ValidRepo(s string) bool {
	m := repoRe.FindStringSubmatch(s)
	return m != nil && m[1] != "." && m[1] != ".."
}

// Repo - репозиторий продукта.
func (c *Client) Repo(product string) string {
	if r := c.Repos[product]; r != "" {
		return r
	}
	if product == stack.ProductMtg {
		return DefaultMtgRepo
	}
	return DefaultTelemtRepo
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

// ghRelease - ответ GitHub API.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// getRelease - релиз по пути API; 404 - ErrReleaseNotFound.
func (c *Client) getRelease(ctx context.Context, product, apiPath string) (Release, error) {
	repo := c.Repo(product)
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	u := c.apiBase() + "/repos/" + repo + "/releases/" + apiPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Release{}, fmt.Errorf("%s: запрос релиза: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("%s: релиз: %w", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Release{}, fmt.Errorf("%s: %w", repo, ErrReleaseNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("%s: релиз: GitHub ответил HTTP %d", repo, resp.StatusCode)
	}
	var raw ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSmall)).Decode(&raw); err != nil {
		return Release{}, fmt.Errorf("%s: разбор релиза: %w", repo, err)
	}
	if raw.TagName == "" {
		return Release{}, fmt.Errorf("%s: в релизе нет tag_name", repo)
	}
	if raw.Draft {
		return Release{}, fmt.Errorf("%s: релиз %s - черновик: %w", repo, raw.TagName, ErrReleaseNotFound)
	}
	if _, err := stack.ParseVersion(raw.TagName); err != nil {
		return Release{}, fmt.Errorf("%s: тег %q: %w", repo, raw.TagName, ErrBadReleaseVersion)
	}
	return Release{Product: product, Repo: repo, Tag: raw.TagName, Version: strings.TrimPrefix(raw.TagName, "v"),
		PublishedAt: raw.PublishedAt.UTC(), Assets: raw.Assets}, nil
}

// Latest - последний релиз продукта (не черновик и не pre-release: /releases/latest).
func (c *Client) Latest(ctx context.Context, product string) (Release, error) {
	if !stack.ValidProduct(product) {
		return Release{}, fmt.Errorf("неизвестный продукт %q", product)
	}
	return c.getRelease(ctx, product, "latest")
}

var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// ValidVersion - версия, которую можно искать как тег: буквы, цифры, . _ + -, до 64 символов.
func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// ByVersion - релиз по версии или тегу: пробуются тег как есть, с ведущей v и без неё.
// Нет ни одного - ErrReleaseNotFound.
func (c *Client) ByVersion(ctx context.Context, product, version string) (Release, error) {
	if !stack.ValidProduct(product) {
		return Release{}, fmt.Errorf("неизвестный продукт %q", product)
	}
	if !ValidVersion(version) {
		return Release{}, fmt.Errorf("версия %q: %w", version, ErrReleaseNotFound)
	}
	bare := strings.TrimPrefix(version, "v")
	tags := []string{version}
	for _, t := range []string{bare, "v" + bare} {
		if bare != "" && !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	var lastErr error
	for _, tag := range tags {
		rel, err := c.getRelease(ctx, product, "tags/"+url.PathEscape(tag))
		if err == nil {
			return rel, nil
		}
		if errors.Is(err, ErrBadReleaseVersion) {
			// Релиз с таким тегом есть, но поставить его нельзя: версию не с чем сравнить.
			return Release{}, fmt.Errorf("%w: %w", ErrReleaseNotFound, err)
		}
		if !errors.Is(err, ErrReleaseNotFound) {
			return Release{}, err
		}
		lastErr = err
	}
	return Release{}, lastErr
}

// Arch - архитектура машины в именах файлов релиза: amd64 или arm64. Пусто - amd64.
func Arch(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "amd64", "x86_64", "x86-64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	}
	return "", fmt.Errorf("%q: %w", s, ErrUnsupportedArch)
}

// Files - имена архива и файла контрольных сумм релиза и имя бинаря в архиве.
type Files struct {
	Archive   string
	Checksums string
	Binary    string
}

// FilesFor - файлы релиза продукта для архитектуры (amd64 | arm64).
//   - telemt: telemt-x86_64-linux-musl.tar.gz (arm64 - telemt-aarch64-linux-musl.tar.gz),
//     сумма в <архив>.sha256, бинарь telemt;
//   - mtg: mtg-multi-<версия>-linux-<arch>.tar.gz, суммы в mtg-multi-<версия>-checksums.txt,
//     бинарь mtg-multi.
func FilesFor(rel Release, arch string) (Files, error) {
	a, err := Arch(arch)
	if err != nil {
		return Files{}, err
	}
	switch rel.Product {
	case stack.ProductTelemt:
		cpu := "x86_64"
		if a == "arm64" {
			cpu = "aarch64"
		}
		archive := "telemt-" + cpu + "-linux-musl.tar.gz"
		return Files{Archive: archive, Checksums: archive + ".sha256", Binary: "telemt"}, nil
	case stack.ProductMtg:
		return Files{Archive: "mtg-multi-" + rel.Version + "-linux-" + a + ".tar.gz",
			Checksums: "mtg-multi-" + rel.Version + "-checksums.txt", Binary: "mtg-multi"}, nil
	}
	return Files{}, fmt.Errorf("неизвестный продукт %q", rel.Product)
}

func pickAsset(rel Release, name string) (Asset, bool) {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// download скачивает файл не больше limit байт. Токен не отправляется: файлы релизов
// публичные, а ссылка уводит на другой домен.
func (c *Client) download(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("запрос %s: %w", rawURL, err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("скачивание %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("скачивание %s: HTTP %d", rawURL, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("скачивание %s: %d байт, больше предела %d", rawURL, resp.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("скачивание %s: %w", rawURL, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("скачивание %s: больше предела %d байт", rawURL, limit)
	}
	return data, nil
}

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// checksumFor - SHA256 файла name из текста контрольных сумм: строки «hex  имя»
// (имя может быть с * - режим binary у sha256sum) или одна строка с hex без имени.
func checksumFor(text, name string) (string, bool) {
	sums := selfupdate.ParseChecksums(text)
	for _, n := range []string{name, "*" + name} {
		if v, ok := sums[n]; ok && hexRe.MatchString(v) {
			return strings.ToLower(v), true
		}
	}
	fields := strings.Fields(text)
	if len(fields) == 1 && hexRe.MatchString(fields[0]) {
		return strings.ToLower(fields[0]), true
	}
	return "", false
}

// SHA256Hex - SHA256 данных в hex.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Binary скачивает архив релиза для архитектуры, сверяет его SHA256 с контрольной суммой
// релиза и достаёт бинарь продукта (по базовому имени, в любом подкаталоге архива).
func (c *Client) Binary(ctx context.Context, rel Release, arch string) ([]byte, error) {
	files, err := FilesFor(rel, arch)
	if err != nil {
		return nil, err
	}
	archive, ok := pickAsset(rel, files.Archive)
	if !ok {
		return nil, fmt.Errorf("%s %s: %w %s", rel.Repo, rel.Tag, ErrAssetMissing, files.Archive)
	}
	sums, ok := pickAsset(rel, files.Checksums)
	if !ok {
		return nil, fmt.Errorf("%s %s: %w %s", rel.Repo, rel.Tag, ErrAssetMissing, files.Checksums)
	}
	sumsRaw, err := c.download(ctx, sums.URL, maxSmall)
	if err != nil {
		return nil, err
	}
	want, ok := checksumFor(string(sumsRaw), files.Archive)
	if !ok {
		return nil, fmt.Errorf("%s %s: в %s нет SHA256 для %s", rel.Repo, rel.Tag, files.Checksums, files.Archive)
	}
	data, err := c.download(ctx, archive.URL, MaxArchive)
	if err != nil {
		return nil, err
	}
	if got := SHA256Hex(data); got != want {
		return nil, fmt.Errorf("%s %s: SHA256 %s не сошлась: ждали %s, получили %s", rel.Repo, rel.Tag, files.Archive, want, got)
	}
	bin, err := extract(data, files.Binary)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %s: %w", rel.Repo, rel.Tag, files.Archive, err)
	}
	return bin, nil
}

// extract достаёт из tar.gz обычный файл с базовым именем name (не больше MaxBinary).
func extract(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("не gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("в архиве нет файла %s", name)
		}
		if err != nil {
			return nil, fmt.Errorf("не tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != name {
			continue
		}
		if h.Size > MaxBinary {
			return nil, fmt.Errorf("%s: %d байт, больше предела %d", name, h.Size, MaxBinary)
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxBinary+1))
		if err != nil {
			return nil, fmt.Errorf("чтение %s: %w", name, err)
		}
		if int64(len(data)) > MaxBinary {
			return nil, fmt.Errorf("%s: больше предела %d байт", name, MaxBinary)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%s: пустой файл", name)
		}
		return data, nil
	}
}
