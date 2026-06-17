package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseChecksums(t *testing.T) {
	txt := "abc123  telemux_linux_amd64\ndef456  telemux_linux_arm64\n"
	m := ParseChecksums(txt)
	if m["telemux_linux_amd64"] != "abc123" || m["telemux_linux_arm64"] != "def456" {
		t.Fatalf("разбор checksums неверен: %v", m)
	}
}

func TestAssetNameAndPick(t *testing.T) {
	if AssetName("linux", "amd64") != "telemux_linux_amd64" {
		t.Fatal("AssetName неверен")
	}
	r := Release{Assets: []Asset{{Name: "checksums.txt"}, {Name: "telemux_linux_amd64", URL: "u"}}}
	a, ok := PickAsset(r, "telemux_linux_amd64")
	if !ok || a.URL != "u" {
		t.Fatalf("PickAsset не нашёл бинарь: %+v %v", a, ok)
	}
	if _, ok := PickAsset(r, "нет-такого"); ok {
		t.Fatal("PickAsset нашёл несуществующий")
	}
}

// releaseServer — стаб GitHub API + раздача артефактов.
func releaseServer(t *testing.T, version string, binData []byte, checksumOverride string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(binData)
	sumHex := hex.EncodeToString(sum[:])
	if checksumOverride != "" {
		sumHex = checksumOverride
	}
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/AndreyOsipuk/telemux/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := map[string]any{
			"tag_name": version,
			"assets": []map[string]string{
				{"name": "telemux_linux_amd64", "browser_download_url": base + "/dl/bin"},
				{"name": "checksums.txt", "browser_download_url": base + "/dl/sums"},
			},
		}
		json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/dl/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(binData) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  telemux_linux_amd64\n", sumHex)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	return srv
}

func TestUpdate_Success(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "telemux")
	os.WriteFile(binPath, []byte("OLD-BINARY"), 0o755)

	newBin := []byte("NEW-BINARY-v0.2.0")
	srv := releaseServer(t, "v0.2.0", newBin, "")
	defer srv.Close()

	from, to, err := Update(context.Background(), Options{
		Owner: "AndreyOsipuk", Repo: "telemux", CurrentVersion: "v0.1.0",
		GOOS: "linux", GOARCH: "amd64", BinaryPath: binPath,
		APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if from != "v0.1.0" || to != "v0.2.0" {
		t.Fatalf("версии: %s → %s", from, to)
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != string(newBin) {
		t.Fatalf("бинарь не подменён: %q", got)
	}
	bak, _ := os.ReadFile(binPath + ".bak")
	if string(bak) != "OLD-BINARY" {
		t.Fatalf("backup не создан: %q", bak)
	}
}

func TestUpdate_ChecksumMismatchRefuses(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "telemux")
	os.WriteFile(binPath, []byte("OLD-BINARY"), 0o755)

	srv := releaseServer(t, "v0.2.0", []byte("NEW"), "deadbeef") // неверный sha
	defer srv.Close()

	_, _, err := Update(context.Background(), Options{
		Owner: "AndreyOsipuk", Repo: "telemux", CurrentVersion: "v0.1.0",
		GOOS: "linux", GOARCH: "amd64", BinaryPath: binPath,
		APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if err == nil {
		t.Fatal("при несовпадении checksum Update должен вернуть ошибку")
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != "OLD-BINARY" {
		t.Fatalf("бинарь НЕ должен меняться при mismatch, стало: %q", got)
	}
}

func TestUpdate_AlreadyLatest(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", []byte("x"), "")
	defer srv.Close()
	from, to, err := Update(context.Background(), Options{
		Owner: "AndreyOsipuk", Repo: "telemux", CurrentVersion: "v0.2.0",
		GOOS: "linux", GOARCH: "amd64", APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil || from != "v0.2.0" || to != "v0.2.0" {
		t.Fatalf("уже последняя: ожидали no-op без ошибки, получили %s→%s err=%v", from, to, err)
	}
}
