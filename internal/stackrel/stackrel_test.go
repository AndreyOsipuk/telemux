package stackrel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tarGz собирает tar.gz из файлов имя -> содержимое (и каталогов с / на конце).
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if strings.HasSuffix(name, "/") {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeGitHub - API релизов и файлы релизов на одном httptest-сервере.
type fakeGitHub struct {
	srv      *httptest.Server
	mu       sync.Mutex
	releases map[string]map[string]any // repo -> tag -> релиз; "latest" - последний
	files    map[string][]byte         // путь -> содержимое
	auth     map[string]string         // путь -> заголовок Authorization
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{releases: map[string]map[string]any{}, files: map[string][]byte{}, auth: map[string]string{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.auth[r.URL.Path] = r.Header.Get("Authorization")
		if body, ok := g.files[r.URL.Path]; ok {
			w.Write(body) //nolint:errcheck
			return
		}
		// /repos/{owner}/{repo}/releases/latest | /releases/tags/{tag}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		if len(parts) >= 4 && parts[2] == "releases" {
			repo := parts[0] + "/" + parts[1]
			key := parts[3]
			if key == "tags" && len(parts) == 5 {
				key = parts[4]
			}
			if rel, ok := g.releases[repo][key]; ok {
				json.NewEncoder(w).Encode(rel) //nolint:errcheck
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// addRelease публикует релиз с файлами; latest - он же последний.
func (g *fakeGitHub) addRelease(repo, tag string, latest bool, files map[string][]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	assets := []map[string]any{}
	for name, body := range files {
		p := "/dl/" + repo + "/" + tag + "/" + name
		g.files[p] = body
		assets = append(assets, map[string]any{"name": name, "browser_download_url": g.srv.URL + p, "size": len(body)})
	}
	rel := map[string]any{"tag_name": tag, "draft": false, "prerelease": false,
		"published_at": "2026-11-08T12:08:51Z", "assets": assets}
	if g.releases[repo] == nil {
		g.releases[repo] = map[string]any{}
	}
	g.releases[repo][tag] = rel
	if latest {
		g.releases[repo]["latest"] = rel
	}
}

func (g *fakeGitHub) client(token string) *Client {
	return &Client{APIBase: g.srv.URL, Token: token, HTTP: g.srv.Client()}
}

// telemtRelease - релиз telemt: архив с бинарём в корне и сумма в .sha256.
func telemtRelease(t *testing.T, g *fakeGitHub, tag, bin, sumFormat string) []byte {
	t.Helper()
	archive := tarGz(t, map[string]string{"telemt": bin})
	sum := SHA256Hex(archive)
	var sumFile string
	switch sumFormat {
	case "named":
		sumFile = sum + "  telemt-x86_64-linux-musl.tar.gz\n"
	case "bare":
		sumFile = sum + "\n"
	case "wrong":
		sumFile = strings.Repeat("0", 64) + "  telemt-x86_64-linux-musl.tar.gz\n"
	}
	g.addRelease("telemt/telemt", tag, true, map[string][]byte{
		"telemt-x86_64-linux-musl.tar.gz":        archive,
		"telemt-x86_64-linux-musl.tar.gz.sha256": []byte(sumFile),
		"telemt-x86_64-linux-gnu.tar.gz":         []byte("не тот"),
	})
	return archive
}

func TestLatestAndBinary_Telemt(t *testing.T) {
	g := newFakeGitHub(t)
	telemtRelease(t, g, "3.5.17", "ELF-telemt", "named")
	c := g.client("gh-token")
	rel, err := c.Latest(context.Background(), "telemt")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "3.5.17" || rel.Version != "3.5.17" || rel.Repo != "telemt/telemt" ||
		!rel.PublishedAt.Equal(time.Date(2026, 11, 8, 12, 8, 51, 0, time.UTC)) {
		t.Fatalf("релиз: %+v", rel)
	}
	bin, err := c.Binary(context.Background(), rel, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(bin) != "ELF-telemt" {
		t.Fatalf("бинарь: %q", bin)
	}
	// Токен - только в API, не при скачивании файлов.
	if g.auth["/repos/telemt/telemt/releases/latest"] != "Bearer gh-token" {
		t.Fatalf("токен в API: %q", g.auth["/repos/telemt/telemt/releases/latest"])
	}
	if a := g.auth["/dl/telemt/telemt/3.5.17/telemt-x86_64-linux-musl.tar.gz"]; a != "" {
		t.Fatalf("токен ушёл при скачивании: %q", a)
	}
}

// Сумма в .sha256 одной строкой hex без имени файла.
func TestBinary_BareChecksum(t *testing.T) {
	g := newFakeGitHub(t)
	telemtRelease(t, g, "3.5.17", "ELF", "bare")
	c := g.client("")
	rel, err := c.Latest(context.Background(), "telemt")
	if err != nil {
		t.Fatal(err)
	}
	if bin, err := c.Binary(context.Background(), rel, "amd64"); err != nil || string(bin) != "ELF" {
		t.Fatalf("бинарь: %q %v", bin, err)
	}
	if g.auth["/repos/telemt/telemt/releases/latest"] != "" {
		t.Fatal("без токена заголовка Authorization быть не должно")
	}
}

func TestBinary_WrongChecksum(t *testing.T) {
	g := newFakeGitHub(t)
	telemtRelease(t, g, "3.5.17", "ELF", "wrong")
	c := g.client("")
	rel, _ := c.Latest(context.Background(), "telemt")
	if _, err := c.Binary(context.Background(), rel, "amd64"); err == nil || !strings.Contains(err.Error(), "не сошлась") {
		t.Fatalf("неверная сумма: ждали ошибку, получили %v", err)
	}
}

// mtg: тег с v, архив с версией в имени, бинарь в подкаталоге, общий файл сумм.
func TestByVersionAndBinary_MtgSubdir(t *testing.T) {
	g := newFakeGitHub(t)
	archive := tarGz(t, map[string]string{
		"mtg-multi-1.15.0-linux-amd64/":           "",
		"mtg-multi-1.15.0-linux-amd64/README.md":  "readme",
		"mtg-multi-1.15.0-linux-amd64/web/x":      "web",
		"mtg-multi-1.15.0-linux-amd64/mtg-multi":  "ELF-mtg",
		"mtg-multi-1.15.0-linux-amd64/LICENSE":    "mit",
		"mtg-multi-1.15.0-linux-amd64/mtg-multi2": "не он",
	})
	sums := "5bf8  mtg-multi-1.15.0-darwin-amd64.tar.gz\n" + SHA256Hex(archive) + "  mtg-multi-1.15.0-linux-amd64.tar.gz\n"
	g.addRelease("dolonet/mtg-multi", "v1.15.0", true, map[string][]byte{
		"mtg-multi-1.15.0-linux-amd64.tar.gz":  archive,
		"mtg-multi-1.15.0-checksums.txt":       []byte(sums),
		"mtg-multi-1.15.0-darwin-amd64.tar.gz": []byte("x"),
	})
	c := g.client("")
	// Версия без v находит тег v1.15.0.
	rel, err := c.ByVersion(context.Background(), "mtg", "1.15.0")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.15.0" || rel.Version != "1.15.0" {
		t.Fatalf("релиз: %+v", rel)
	}
	if rel2, err := c.ByVersion(context.Background(), "mtg", "v1.15.0"); err != nil || rel2.Tag != "v1.15.0" {
		t.Fatalf("по тегу: %+v %v", rel2, err)
	}
	bin, err := c.Binary(context.Background(), rel, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if string(bin) != "ELF-mtg" {
		t.Fatalf("бинарь: %q", bin)
	}
	// arm64: в релизе нет файла.
	if _, err := c.Binary(context.Background(), rel, "arm64"); err == nil || !strings.Contains(err.Error(), "нет файла mtg-multi-1.15.0-linux-arm64.tar.gz") {
		t.Fatalf("нет архива arm64: %v", err)
	}
}

func TestByVersion_NotFound(t *testing.T) {
	g := newFakeGitHub(t)
	telemtRelease(t, g, "3.5.17", "ELF", "named")
	c := g.client("")
	if _, err := c.ByVersion(context.Background(), "telemt", "9.9.9"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("нет релиза: %v", err)
	}
	if _, err := c.ByVersion(context.Background(), "telemt", "../../x"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("версия не по формату: %v", err)
	}
	// Тег без v находится и по версии с v.
	if rel, err := c.ByVersion(context.Background(), "telemt", "v3.5.17"); err != nil || rel.Tag != "3.5.17" {
		t.Fatalf("v3.5.17: %+v %v", rel, err)
	}
}

func TestErrors_NoFiles(t *testing.T) {
	g := newFakeGitHub(t)
	// Нет файла сумм.
	archive := tarGz(t, map[string]string{"telemt": "ELF"})
	g.addRelease("telemt/telemt", "3.5.18", true, map[string][]byte{"telemt-x86_64-linux-musl.tar.gz": archive})
	c := g.client("")
	rel, _ := c.Latest(context.Background(), "telemt")
	if _, err := c.Binary(context.Background(), rel, "amd64"); err == nil || !strings.Contains(err.Error(), ".sha256") {
		t.Fatalf("нет файла сумм: %v", err)
	}
	// В архиве нет бинаря.
	archive = tarGz(t, map[string]string{"README": "x"})
	g.addRelease("telemt/telemt", "3.5.19", true, map[string][]byte{
		"telemt-x86_64-linux-musl.tar.gz":        archive,
		"telemt-x86_64-linux-musl.tar.gz.sha256": []byte(SHA256Hex(archive)),
	})
	rel, _ = c.Latest(context.Background(), "telemt")
	if _, err := c.Binary(context.Background(), rel, "amd64"); err == nil || !strings.Contains(err.Error(), "нет файла telemt") {
		t.Fatalf("нет бинаря в архиве: %v", err)
	}
	// Архив больше предела.
	old := MaxArchive
	MaxArchive = 10
	defer func() { MaxArchive = old }()
	if _, err := c.Binary(context.Background(), rel, "amd64"); err == nil || !strings.Contains(err.Error(), "предела") {
		t.Fatalf("большой архив: %v", err)
	}
	// Нет репозитория - ErrReleaseNotFound, GitHub недоступен - другая ошибка.
	c.Repos = map[string]string{"telemt": "nobody/none"}
	if _, err := c.Latest(context.Background(), "telemt"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("нет репозитория: %v", err)
	}
	g.srv.Close()
	if _, err := c.Latest(context.Background(), "telemt"); err == nil || errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("GitHub недоступен: %v", err)
	}
}

func TestArchAndFiles(t *testing.T) {
	for in, want := range map[string]string{"": "amd64", "x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"} {
		if got, err := Arch(in); err != nil || got != want {
			t.Fatalf("Arch(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := Arch("riscv64"); !errors.Is(err, ErrUnsupportedArch) {
		t.Fatalf("riscv64: %v", err)
	}
	f, _ := FilesFor(Release{Product: "telemt", Version: "3.5.17"}, "arm64")
	if f.Archive != "telemt-aarch64-linux-musl.tar.gz" || f.Checksums != "telemt-aarch64-linux-musl.tar.gz.sha256" || f.Binary != "telemt" {
		t.Fatalf("telemt arm64: %+v", f)
	}
	f, _ = FilesFor(Release{Product: "mtg", Version: "1.15.0"}, "amd64")
	if f.Archive != "mtg-multi-1.15.0-linux-amd64.tar.gz" || f.Checksums != "mtg-multi-1.15.0-checksums.txt" || f.Binary != "mtg-multi" {
		t.Fatalf("mtg amd64: %+v", f)
	}
	c := &Client{}
	if c.Repo("telemt") != DefaultTelemtRepo || c.Repo("mtg") != DefaultMtgRepo {
		t.Fatal("репозитории по умолчанию")
	}
	if !ValidRepo("owner/repo-1.x") || ValidRepo("owner") || ValidRepo("a/b/c") || ValidRepo("../x") {
		t.Fatal("ValidRepo")
	}
}

func TestChecksumFor(t *testing.T) {
	h := strings.Repeat("ab", 32)
	for text, ok := range map[string]bool{
		h + "  f.tar.gz\n":                   true,
		h + " *f.tar.gz\n":                   true,
		strings.ToUpper(h) + "\n":            true,
		h + "  other.tar.gz\n":               false,
		"zz  f.tar.gz\n":                     false,
		h + "  other\n" + h + "  f.tar.gz\n": true,
		"":                                   false,
	} {
		got, found := checksumFor(text, "f.tar.gz")
		if found != ok || (ok && got != h) {
			t.Fatalf("checksumFor(%q) = %q, %v", text, got, found)
		}
	}
}

// Кэш: файл = sha256, повторная запись - тот же файл, временных файлов не остаётся.
func TestCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stack")
	c := Cache{Dir: dir}
	sha, size, err := c.Put([]byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	if sha != SHA256Hex([]byte("binary")) || size != 6 {
		t.Fatalf("Put: %s %d", sha, size)
	}
	if _, _, err := c.Put([]byte("binary")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != sha {
		t.Fatalf("в каталоге: %v", entries)
	}
	f, n, err := c.Open(sha)
	if err != nil || n != 6 {
		t.Fatalf("Open: %v %d", err, n)
	}
	f.Close()
	if _, _, err := c.Open(strings.Repeat("0", 64)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("нет файла: %v", err)
	}
	if _, _, err := c.Open("../etc/passwd"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("путь вне кэша: %v", err)
	}
	if _, _, err := c.Put(nil); err == nil {
		t.Fatal("пустой бинарь")
	}
}

// Тег, из которого не разобрать версию X.Y.Z (например "v"), - ошибка релиза, а не релиз
// с пустой версией: для latest - ошибка проверки, для явной версии - ErrReleaseNotFound.
func TestRelease_BadTag(t *testing.T) {
	g := newFakeGitHub(t)
	g.addRelease("telemt/telemt", "v", true, map[string][]byte{})
	g.addRelease("telemt/telemt", "nightly", false, map[string][]byte{})
	c := g.client("")
	if _, err := c.Latest(context.Background(), "telemt"); !errors.Is(err, ErrBadReleaseVersion) {
		t.Fatalf("latest с тегом v: %v", err)
	}
	for _, v := range []string{"v", "nightly"} {
		if _, err := c.ByVersion(context.Background(), "telemt", v); !errors.Is(err, ErrReleaseNotFound) {
			t.Fatalf("ByVersion(%q): %v", v, err)
		}
	}
}

// Нет архива под архитектуру - ErrAssetMissing; неизвестная архитектура - ErrUnsupportedArch.
func TestBinary_AssetMissing(t *testing.T) {
	g := newFakeGitHub(t)
	telemtRelease(t, g, "3.5.17", "ELF", "named")
	c := g.client("")
	rel, _ := c.Latest(context.Background(), "telemt")
	if _, err := c.Binary(context.Background(), rel, "arm64"); !errors.Is(err, ErrAssetMissing) {
		t.Fatalf("arm64: %v", err)
	}
	if _, err := c.Binary(context.Background(), rel, "riscv64"); !errors.Is(err, ErrUnsupportedArch) {
		t.Fatalf("riscv64: %v", err)
	}
}

// Кэш перезаписывает испорченный файл с тем же именем, Verify сверяет хэш.
func TestCache_RewritesCorrupted(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	sha, _, err := c.Put([]byte("binary"))
	if err != nil || !c.Verify(sha) {
		t.Fatalf("Put: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, sha), []byte("bin4ry"), 0o640); err != nil {
		t.Fatal(err)
	}
	if c.Verify(sha) {
		t.Fatal("испорченный файл того же размера прошёл проверку")
	}
	if _, _, err := c.Put([]byte("binary")); err != nil || !c.Verify(sha) {
		t.Fatalf("перезапись: %v", err)
	}
	if c.Verify(strings.Repeat("0", 64)) {
		t.Fatal("нет файла")
	}
}
