package tlspin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Выпущенный сертификат читается, отпечаток совпадает с тем, что видит клиент.
func TestGenerateAndPin(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	fp, err := Generate(certFile, keyFile, "telemux-master")
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 64 {
		t.Fatalf("отпечаток - 64 hex, получили %q", fp)
	}
	if st, _ := os.Stat(keyFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("ключ должен быть 0600, а %v", st.Mode().Perm())
	}
	if got, err := FingerprintFile(certFile); err != nil || got != fp {
		t.Fatalf("отпечаток из файла %q, ждали %q, %v", got, fp, err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	var hits int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	ok := &http.Client{Transport: &http.Transport{TLSClientConfig: Config(fp)}}
	if resp, err := ok.Get(srv.URL); err != nil {
		t.Fatalf("верный отпечаток не пустил: %v", err)
	} else {
		resp.Body.Close()
	}
	bad := &http.Client{Transport: &http.Transport{TLSClientConfig: Config(strings.Repeat("0", 64))}}
	if _, err := bad.Get(srv.URL); err == nil {
		t.Fatal("неверный отпечаток пустил")
	}
	if hits != 1 {
		t.Fatalf("при неверном сертификате запрос дошёл до сервера: %d", hits)
	}
}

// Отпечаток можно писать с двоеточиями и в любом регистре.
func TestNormalize(t *testing.T) {
	if Normalize("AB:cd:EF") != "abcdef" {
		t.Fatal("отпечаток не нормализован")
	}
}
