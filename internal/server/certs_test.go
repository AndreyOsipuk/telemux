package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/acme"
	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

// fakeCerts - таблица tls_certs и машины с доменами в памяти.
type fakeCerts struct {
	mu        sync.Mutex
	targets   []store.TLSTarget
	certs     map[string]*fakeCertRow
	published []string // машины, которым вызывалась публикация
	changed   bool     // публикация даёт новую ревизию
	calls     int      // любые обращения к базе
	list      []store.CertInfo
}

type fakeCertRow struct {
	cert, key []byte
	notAfter  *time.Time
	lastTry   *time.Time
	lastErr   string
}

func newFakeCerts(targets ...store.TLSTarget) *fakeCerts {
	return &fakeCerts{targets: targets, certs: map[string]*fakeCertRow{}}
}

func (f *fakeCerts) TLSTargets(context.Context) ([]store.TLSTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return slices.Clone(f.targets), nil
}

func (f *fakeCerts) TLSCertStates(_ context.Context, domains []string) (map[string]store.TLSCertState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := map[string]store.TLSCertState{}
	for _, d := range domains {
		if r, ok := f.certs[d]; ok {
			out[d] = store.TLSCertState{Domain: d, HasCert: r.cert != nil, NotAfter: r.notAfter, LastTryAt: r.lastTry, LastError: r.lastErr}
		}
	}
	return out, nil
}

func (f *fakeCerts) SaveTLSCert(_ context.Context, domain string, cert, key []byte, notAfter time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	now := time.Now()
	f.certs[domain] = &fakeCertRow{cert: cert, key: key, notAfter: &notAfter, lastTry: &now}
	return nil
}

func (f *fakeCerts) RecordTLSError(_ context.Context, domain, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	r, ok := f.certs[domain]
	if !ok {
		r = &fakeCertRow{}
		f.certs[domain] = r
	}
	now := time.Now()
	r.lastTry, r.lastErr = &now, msg
	return nil
}

func (f *fakeCerts) PublishTLSWeb(_ context.Context, target string) (store.PublishResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.published = append(f.published, target)
	if f.changed {
		return store.PublishResult{Revision: 7, Changed: []string{target}}, nil
	}
	return store.PublishResult{Revision: 7, Changed: []string{}}, nil
}

func (f *fakeCerts) ListCerts(_ context.Context, limit, offset int) ([]store.CertInfo, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return page(f.list, limit, offset), len(f.list), nil
}

// fakeIssuer - выпуск без сети: сертификат на 90 дней от «сейчас» теста.
type fakeIssuer struct {
	mu     sync.Mutex
	now    func() time.Time
	issued []string
	fail   map[string]error
}

func (f *fakeIssuer) Issue(_ context.Context, domain string) (acme.Cert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued = append(f.issued, domain)
	if err := f.fail[domain]; err != nil {
		return acme.Cert{}, err
	}
	return acme.Cert{CertPEM: []byte("CERT " + domain), KeyPEM: []byte("KEY " + domain), NotAfter: f.now().Add(90 * 24 * time.Hour)}, nil
}

// clock - подменяемое время теста.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func certsServer(inRecovery bool, fc *fakeCerts) (*Server, *fakeIssuer, *clock) {
	clk := &clock{t: time.Now()}
	iss := &fakeIssuer{now: clk.now, fail: map[string]error{}}
	s := New(Deps{
		Store: fakeStore{inRecovery: inRecovery}, Node: &fakeNode{}, Version: "v1",
		SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}, BotToken: botToken,
		AdminUser: "admin", AdminPassword: "pw",
		Certs: fc,
		ACME:  &ACMEDeps{Issuer: iss, Now: clk.now},
	})
	return s, iss, clk
}

func relayTLS(code string, assigned bool, domains ...string) store.TLSTarget {
	return store.TLSTarget{Code: code, Domains: domains, Assigned: assigned}
}

// Новый домен выпускается, свежий не трогается, за 29 дней до конца - продлевается.
func TestCerts_IssueNewSkipFreshRenewDue(t *testing.T) {
	fc := newFakeCerts(relayTLS("pr1", true, "web1.example.com", "web5.example.com"))
	s, iss, clk := certsServer(false, fc)
	fresh := clk.now().Add(60 * 24 * time.Hour)
	fc.certs["web5.example.com"] = &fakeCertRow{cert: []byte("old"), key: []byte("old"), notAfter: &fresh}

	s.certsOnce(context.Background())
	if !slices.Equal(iss.issued, []string{"web1.example.com"}) {
		t.Fatalf("ждали выпуск только нового домена, выпущены %v", iss.issued)
	}
	if r := fc.certs["web1.example.com"]; r == nil || string(r.cert) != "CERT web1.example.com" {
		t.Fatalf("сертификат нового домена не сохранён: %+v", r)
	}
	if string(fc.certs["web5.example.com"].cert) != "old" {
		t.Fatal("свежий сертификат перевыпущен")
	}

	// Проход через час: всё свежее - выпусков нет.
	clk.add(time.Hour)
	s.certsOnce(context.Background())
	if len(iss.issued) != 1 {
		t.Fatalf("свежие сертификаты перевыпущены: %v", iss.issued)
	}

	// До конца web5 осталось 29 дней (меньше 30) - продление.
	due := clk.now().Add(29 * 24 * time.Hour)
	fc.certs["web5.example.com"].notAfter = &due
	fc.certs["web5.example.com"].lastTry = nil
	s.certsOnce(context.Background())
	if !slices.Equal(iss.issued, []string{"web1.example.com", "web5.example.com"}) {
		t.Fatalf("ждали продление web5, выпущены %v", iss.issued)
	}
	if string(fc.certs["web5.example.com"].cert) != "CERT web5.example.com" {
		t.Fatal("продлённый сертификат не сохранён")
	}
}

// Неудача пишет last_error; повтор раньше 6 часов не делается, после - делается.
func TestCerts_FailureRecordedAndThrottled(t *testing.T) {
	fc := newFakeCerts(relayTLS("pr1", true, "web1.example.com"))
	s, iss, clk := certsServer(false, fc)
	iss.fail["web1.example.com"] = errors.New("acme: rate limited")

	s.certsOnce(context.Background())
	r := fc.certs["web1.example.com"]
	if r == nil || !strings.Contains(r.lastErr, "rate limited") || r.lastTry == nil {
		t.Fatalf("ошибка выпуска не записана: %+v", r)
	}
	if r.cert != nil {
		t.Fatal("при ошибке сертификата быть не должно")
	}
	// В фейке last_try_at ставится реальным временем; часы теста идут от того же «сейчас».
	clk.add(5 * time.Hour)
	s.certsOnce(context.Background())
	if len(iss.issued) != 1 {
		t.Fatalf("повтор раньше 6 часов: выпуски %v", iss.issued)
	}
	clk.add(61 * time.Minute)
	delete(iss.fail, "web1.example.com")
	s.certsOnce(context.Background())
	if len(iss.issued) != 2 || fc.certs["web1.example.com"].cert == nil {
		t.Fatalf("после 6 часов ждали повтор и сертификат: выпуски %v", iss.issued)
	}
	if fc.certs["web1.example.com"].lastErr != "" {
		t.Fatal("после успешного выпуска ошибка должна сброситься")
	}
}

// Вне роли мастера цикл ничего не делает: ни базы, ни выпусков.
func TestCerts_NotMasterDoesNothing(t *testing.T) {
	fc := newFakeCerts(relayTLS("pr1", true, "web1.example.com"))
	s, iss, _ := certsServer(true, fc)
	s.certsOnce(context.Background())
	if fc.calls != 0 || len(iss.issued) != 0 {
		t.Fatalf("на реплике цикл работал: обращений к базе %d, выпусков %v", fc.calls, iss.issued)
	}
}

// tls.web публикуется только машинам, которым он назначен; новая ревизия будит агентов.
func TestCerts_PublishOnlyAssigned(t *testing.T) {
	fc := newFakeCerts(relayTLS("pr1", true, "web1.example.com"), relayTLS("pr2", false, "web2.example.com"),
		relayTLS("pr3", true))
	fc.changed = true
	s, iss, _ := certsServer(false, fc)
	woken := s.revs.wait()
	s.certsOnce(context.Background())
	if !slices.Equal(fc.published, []string{"pr1"}) {
		t.Fatalf("ждали публикацию только pr1, получили %v", fc.published)
	}
	if !slices.Contains(iss.issued, "web2.example.com") {
		t.Fatal("домены релея без tls.web тоже выпускаются - сертификат готов к переводу машины")
	}
	select {
	case <-woken:
	default:
		t.Fatal("новая ревизия tls.web не разбудила long-poll агентов")
	}
}

// Машина с мусором в web_domains пропускается, остальные обрабатываются.
func TestCerts_InvalidSettingsSkipped(t *testing.T) {
	bad := store.TLSTarget{Code: "pr4", Assigned: true, Invalid: "домен *.example.com"}
	fc := newFakeCerts(bad, relayTLS("pr1", true, "web1.example.com"))
	s, iss, _ := certsServer(false, fc)
	s.certsOnce(context.Background())
	if !slices.Equal(iss.issued, []string{"web1.example.com"}) || !slices.Equal(fc.published, []string{"pr1"}) {
		t.Fatalf("выпуски %v, публикации %v", iss.issued, fc.published)
	}
}

// Один домен на двух релеях выпускается один раз.
func TestCerts_SharedDomainIssuedOnce(t *testing.T) {
	fc := newFakeCerts(relayTLS("pr1", true, "web1.example.com"), relayTLS("pr2", true, "web1.example.com"))
	s, iss, _ := certsServer(false, fc)
	s.certsOnce(context.Background())
	if len(iss.issued) != 1 || len(fc.published) != 2 {
		t.Fatalf("выпуски %v, публикации %v", iss.issued, fc.published)
	}
}

// GET /api/v1/certs: 401 без токена, постранично, без ключей.
func TestCerts_ListAPI(t *testing.T) {
	fc := newFakeCerts()
	na := time.Now().Add(50 * 24 * time.Hour).UTC()
	for _, d := range []string{"web1.example.com", "web5.example.com", "web2.example.com"} {
		fc.list = append(fc.list, store.CertInfo{Machine: "pr1", Domain: d, Assigned: true, NotAfter: &na})
	}
	fc.list[2].LastError = "нет зоны"
	fc.list[2].NotAfter = nil
	s, _, _ := certsServer(false, fc)

	if rec := do(s, "GET", "/api/v1/certs", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена → ждали 401, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/v1/certs", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("чужой токен → ждали 401, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/v1/certs?limit=101", botToken, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=101 → ждали 400, получили %d", rec.Code)
	}
	rec := do(s, "GET", "/api/v1/certs?limit=2&offset=1", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data   []map[string]any `json:"data"`
		Paging map[string]int   `json:"paging"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Paging["total"] != 3 || out.Paging["limit"] != 2 || out.Paging["offset"] != 1 || len(out.Data) != 2 {
		t.Fatalf("пагинация: %s", rec.Body.String())
	}
	if out.Data[0]["domain"] != "web5.example.com" || out.Data[1]["last_error"] != "нет зоны" || out.Data[1]["not_after"] != nil {
		t.Fatalf("строки: %s", rec.Body.String())
	}
	for _, row := range out.Data {
		for k := range row {
			if strings.Contains(k, "key") || strings.Contains(k, "pem") || k == "cert" {
				t.Fatalf("в ответе поле %q: ключи и сертификаты наружу не отдаются", k)
			}
		}
	}
}

// Без ACME (настройки не заданы) список работает, цикл не запускается.
func TestCerts_ListWithoutACME(t *testing.T) {
	fc := newFakeCerts()
	s := New(Deps{Store: fakeStore{}, Node: &fakeNode{}, SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		BotToken: botToken, Certs: fc})
	if rec := do(s, "GET", "/api/v1/certs", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d", rec.Code)
	}
	s.certsOnce(context.Background())
	if fc.calls != 1 {
		t.Fatalf("без ACME цикл не должен ходить в базу: обращений %d", fc.calls)
	}
}

// tls.web собирает сам мастер: извне (бот, панель) он не публикуется.
func TestCerts_TLSWebNotPublishableExternally(t *testing.T) {
	s, _ := v1Server(false, 0)
	rec := do(s, "PUT", "/api/v1/targets/pr2/resources/tls.web", botToken, `{"web2.example.com":{"cert":"x","key":"y"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ждали 400, получили %d: %s", rec.Code, rec.Body.String())
	}
}
