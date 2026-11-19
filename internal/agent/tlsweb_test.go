package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// genCert - самоподписанный сертификат домена (ECDSA P-256) для тестов.
func genCert(t *testing.T, domain string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

func tlsContent(t *testing.T, entries map[string][2]string) []byte {
	t.Helper()
	m := map[string]store.TLSWebEntry{}
	for d, e := range entries {
		m[d] = store.TLSWebEntry{Cert: e[0], Key: e[1]}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeNginx - nginx -t и reload без процесса; testErr - провал проверки конфига.
type fakeNginx struct {
	tests, reloads int
	testErr        error
	reloadErr      error
	onTest         func() // что видит nginx -t в момент проверки
}

func (n *fakeNginx) Test(context.Context) error {
	n.tests++
	if n.onTest != nil {
		n.onTest()
	}
	return n.testErr
}

func (n *fakeNginx) Reload(context.Context) error {
	n.reloads++
	return n.reloadErr
}

func newTLSApplier(dir string, ng *fakeNginx) *TLSApplier {
	return &TLSApplier{Dir: dir, Nginx: ng, FreeSpace: func(string) (uint64, error) { return 1 << 30, nil }}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// Файлы домена: <домен>.pem и <домен>.key с правами 0600, затем nginx -t и reload.
func TestTLSApply_WritesFilesAndReloads(t *testing.T) {
	dir := t.TempDir()
	c1, k1 := genCert(t, "web1.example.com")
	c5, k5 := genCert(t, "web5.example.com")
	ng := &fakeNginx{}
	ap := newTLSApplier(dir, ng)
	r := ap.Apply(context.Background(), tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}, "web5.example.com": {c5, k5}}))
	if r.Status != "applied" {
		t.Fatalf("ждали applied, получили %+v", r)
	}
	for d, want := range map[string][2]string{"web1.example.com": {c1, k1}, "web5.example.com": {c5, k5}} {
		if got := readFile(t, filepath.Join(dir, d+".pem")); got != want[0] {
			t.Fatalf("%s.pem не тот", d)
		}
		if got := readFile(t, filepath.Join(dir, d+".key")); got != want[1] {
			t.Fatalf("%s.key не тот", d)
		}
		if m := mode(t, filepath.Join(dir, d+".key")); m != 0o600 {
			t.Fatalf("права ключа %s: %o, ждали 0600", d, m)
		}
	}
	if ng.tests != 1 || ng.reloads != 1 {
		t.Fatalf("ждали одну проверку и одну перезагрузку, получили %d/%d", ng.tests, ng.reloads)
	}
	// Повтор с тем же содержимым: файлы совпадают - nginx не трогается.
	r = ap.Apply(context.Background(), tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}, "web5.example.com": {c5, k5}}))
	if r.Status != "applied" || ng.tests != 1 || ng.reloads != 1 {
		t.Fatalf("без изменений nginx трогать не нужно: %+v, %d/%d", r, ng.tests, ng.reloads)
	}
}

// Права ключа - 0600, даже если прежний файл был открыт всем.
func TestTLSApply_KeyModeFixed(t *testing.T) {
	dir := t.TempDir()
	c1, k1 := genCert(t, "web1.example.com")
	if err := os.WriteFile(filepath.Join(dir, "web1.example.com.key"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web1.example.com.key"+tmpSuffix), []byte("мусор"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newTLSApplier(dir, &fakeNginx{}).Apply(context.Background(), tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}}))
	if r.Status != "applied" {
		t.Fatalf("%+v", r)
	}
	if m := mode(t, filepath.Join(dir, "web1.example.com.key")); m != 0o600 {
		t.Fatalf("права ключа %o, ждали 0600", m)
	}
	if m := mode(t, filepath.Join(dir, "web1.example.com.key"+bakSuffix)); m != 0o600 {
		t.Fatalf("права бэкапа ключа %o, ждали 0600", m)
	}
}

// nginx -t упал - прежние файлы на месте, новых нет, reload не было, причина в отчёте.
func TestTLSApply_RollbackOnNginxTestFailure(t *testing.T) {
	dir := t.TempDir()
	oc, ok := genCert(t, "web1.example.com")
	writeTestFile(t, filepath.Join(dir, "web1.example.com.pem"), oc)
	writeTestFile(t, filepath.Join(dir, "web1.example.com.key"), ok)
	c1, k1 := genCert(t, "web1.example.com")
	c5, k5 := genCert(t, "web5.example.com")
	ng := &fakeNginx{testErr: errors.New("nginx: [emerg] cannot load certificate")}
	sawNew := false
	ng.onTest = func() { sawNew = readFile(t, filepath.Join(dir, "web1.example.com.pem")) == c1 }

	r := newTLSApplier(dir, ng).Apply(context.Background(), tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}, "web5.example.com": {c5, k5}}))
	if r.Status != "failed" || !strings.Contains(r.Error, "cannot load certificate") {
		t.Fatalf("ждали failed с причиной nginx -t, получили %+v", r)
	}
	if !sawNew {
		t.Fatal("nginx -t должен проверять уже новые файлы")
	}
	if readFile(t, filepath.Join(dir, "web1.example.com.pem")) != oc || readFile(t, filepath.Join(dir, "web1.example.com.key")) != ok {
		t.Fatal("прежние файлы не возвращены")
	}
	for _, f := range []string{"web5.example.com.pem", "web5.example.com.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Fatalf("%s нового домена остался после отката", f)
		}
	}
	if ng.reloads != 0 {
		t.Fatal("после провала nginx -t перезагрузки быть не должно")
	}
}

// reload не прошёл - тоже откат.
func TestTLSApply_RollbackOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	oc, ok := genCert(t, "web1.example.com")
	writeTestFile(t, filepath.Join(dir, "web1.example.com.pem"), oc)
	writeTestFile(t, filepath.Join(dir, "web1.example.com.key"), ok)
	c1, k1 := genCert(t, "web1.example.com")
	ng := &fakeNginx{reloadErr: errors.New("Job for nginx.service failed")}
	r := newTLSApplier(dir, ng).Apply(context.Background(), tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}}))
	if r.Status != "failed" || !strings.Contains(r.Error, "nginx.service failed") {
		t.Fatalf("%+v", r)
	}
	if readFile(t, filepath.Join(dir, "web1.example.com.pem")) != oc {
		t.Fatal("прежний сертификат не возвращён")
	}
}

// Домены, которых нет в ресурсе, не трогаются и не удаляются.
func TestTLSApply_ForeignDomainsUntouched(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "files.example.net.pem"), "чужой сертификат")
	writeTestFile(t, filepath.Join(dir, "files.example.net.key"), "чужой ключ")
	writeTestFile(t, filepath.Join(dir, "default.conf"), "server {}")
	c1, k1 := genCert(t, "web1.example.com")
	r := newTLSApplier(dir, &fakeNginx{}).Apply(context.Background(), tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}}))
	if r.Status != "applied" {
		t.Fatalf("%+v", r)
	}
	for f, want := range map[string]string{"files.example.net.pem": "чужой сертификат", "files.example.net.key": "чужой ключ", "default.conf": "server {}"} {
		if readFile(t, filepath.Join(dir, f)) != want {
			t.Fatalf("%s тронут", f)
		}
	}
}

// Битое содержимое и чужие имена отвергаются до записи.
func TestTLSApply_RejectsBadContent(t *testing.T) {
	c1, k1 := genCert(t, "web1.example.com")
	_, kOther := genCert(t, "web1.example.com")
	c5, k5 := genCert(t, "web5.example.com")
	cases := map[string][]byte{
		"не JSON":                []byte("not json"),
		"путь вместо домена":     tlsContent(t, map[string][2]string{"../../etc/x": {c1, k1}}),
		"звёздочка":              tlsContent(t, map[string][2]string{"*.example.com": {c1, k1}}),
		"ключ от другого серт.":  tlsContent(t, map[string][2]string{"web1.example.com": {c1, kOther}}),
		"сертификат не на домен": tlsContent(t, map[string][2]string{"web1.example.com": {c5, k5}}),
		"пустой сертификат":      tlsContent(t, map[string][2]string{"web1.example.com": {"", k1}}),
	}
	for name, content := range cases {
		dir := t.TempDir()
		ng := &fakeNginx{}
		r := newTLSApplier(dir, ng).Apply(context.Background(), content)
		if r.Status != "failed" {
			t.Fatalf("%s: ждали failed, получили %+v", name, r)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 || ng.tests+ng.reloads != 0 {
			t.Fatalf("%s: до проверки содержимого ничего не пишется и nginx не трогается", name)
		}
	}
}

// Сравнение: какие домены отличаются по sha256 сертификата; ничего не пишет.
func TestCompareTLSWeb(t *testing.T) {
	dir := t.TempDir()
	c1, k1 := genCert(t, "web1.example.com")
	c5, k5 := genCert(t, "web5.example.com")
	oc, _ := genCert(t, "web5.example.com")
	c2, k2 := genCert(t, "web2.example.com")
	writeTestFile(t, filepath.Join(dir, "web1.example.com.pem"), c1)
	writeTestFile(t, filepath.Join(dir, "web1.example.com.key"), k1)
	writeTestFile(t, filepath.Join(dir, "web5.example.com.pem"), oc)
	content := tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}, "web5.example.com": {c5, k5}, "web2.example.com": {c2, k2}})
	before := dirSnapshot(t, dir)
	diff, err := CompareTLSWeb(dir, content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "web5.example.com") || !strings.Contains(diff, "web2.example.com") || strings.Contains(diff, "web1.example.com") {
		t.Fatalf("описание расхождения: %q", diff)
	}
	if dirSnapshot(t, dir) != before {
		t.Fatal("сравнение изменило каталог")
	}
	same, err := CompareTLSWeb(dir, tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}}))
	if err != nil || same != "" {
		t.Fatalf("совпадающие файлы: %q, %v", same, err)
	}
}

// Агент в режиме compare с ресурсом tls.web: отчёт compared с расхождением, каталог не тронут.
func TestAgent_TLSWebCompareWritesNothing(t *testing.T) {
	dir := t.TempDir()
	c1, k1 := genCert(t, "web1.example.com")
	content := tlsContent(t, map[string][2]string{"web1.example.com": {c1, k1}})
	m := &fakeMaster{bundle: store.Bundle{Target: "pr2", Revision: 4, Resources: []store.Desired{
		{Resource: store.ResourceTLSWeb, Revision: 4, SHA256: "x", Content: content},
	}}}
	srv := httptest.NewServer(m.handler(t, "tok", "pr2"))
	defer srv.Close()
	a, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", TLSDir: dir,
		Mode: ModeCompare, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := m.reports[len(m.reports)-1]
	if len(rep.Resources) != 1 || rep.Resources[0].Resource != store.ResourceTLSWeb ||
		rep.Resources[0].Status != "compared" || !strings.Contains(rep.Resources[0].Error, "web1.example.com") {
		t.Fatalf("отчёт: %+v", rep.Resources)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("режим сравнения записал файлы")
	}

	// Без флага tls.web агент ресурс не ведёт, даже если он в бандле.
	b, err := New(Config{MasterURLs: []string{srv.URL}, Token: "tok", Target: "pr2", Mode: ModeCompare, Wait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rep := m.reports[len(m.reports)-1]; len(rep.Resources) != 0 {
		t.Fatalf("агент без TLSDir отчитался о tls.web: %+v", rep.Resources)
	}
	if _, err := New(Config{MasterURLs: []string{"http://m"}, Token: "t", Target: "pr2", TLSDir: dir, Mode: ModeApply}); err == nil {
		t.Fatal("apply с tls.web без TLSApplier принят")
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Name() + "=" + readFile(t, filepath.Join(dir, e.Name())) + ";")
	}
	return b.String()
}
