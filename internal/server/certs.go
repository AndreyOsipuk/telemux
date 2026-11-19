package server

// Сертификаты WEB-доменов релеев (ADR 0004). Мастер выпускает и продлевает их
// по ACME DNS-01 через Cloudflare и публикует машинам ресурс tls.web; агенты
// раскладывают файлы для nginx. Состояние выпуска - в базе (tls_certs), поэтому
// после смены мастера новый мастер продолжает с того же места.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/acme"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// CertStore - сертификаты и их публикация (реализует store.Store).
type CertStore interface {
	TLSTargets(ctx context.Context) ([]store.TLSTarget, error)
	TLSCertStates(ctx context.Context, domains []string) (map[string]store.TLSCertState, error)
	SaveTLSCert(ctx context.Context, domain string, certPEM, keyPEM []byte, notAfter time.Time) error
	RecordTLSError(ctx context.Context, domain, msg string) error
	PublishTLSWeb(ctx context.Context, target string) (store.PublishResult, error)
	ListCerts(ctx context.Context, machine string, limit, offset int) ([]store.CertInfo, int, error)
	CertCounts(ctx context.Context, before time.Time) (store.CertCounts, error)
}

// ACMEDeps - выпуск сертификатов на мастере; nil в Deps - выпуск выключен.
type ACMEDeps struct {
	Issuer       acme.Issuer
	RenewBefore  time.Duration    // продлевать, когда до конца меньше; по умолчанию 30 дней
	RetryAfter   time.Duration    // не пробовать домен чаще; по умолчанию 6 часов (лимиты Let's Encrypt)
	Every        time.Duration    // период прохода; по умолчанию 1 час
	IssueTimeout time.Duration    // на выпуск одного домена; по умолчанию 10 минут
	Now          func() time.Time // для тестов
}

const (
	defaultRenewBefore  = 30 * 24 * time.Hour
	defaultCertsRetry   = 6 * time.Hour
	defaultCertsEvery   = time.Hour
	defaultIssueTimeout = 10 * time.Minute
)

func (d *ACMEDeps) defaults() {
	if d.RenewBefore <= 0 {
		d.RenewBefore = defaultRenewBefore
	}
	if d.RetryAfter <= 0 {
		d.RetryAfter = defaultCertsRetry
	}
	if d.Every <= 0 {
		d.Every = defaultCertsEvery
	}
	if d.IssueTimeout <= 0 {
		d.IssueTimeout = defaultIssueTimeout
	}
	if d.Now == nil {
		d.Now = time.Now
	}
}

// certDue - нужен ли выпуск: сертификата нет или до конца меньше renewBefore,
// и прошлая попытка (удачная или нет) была не позже retryAfter назад.
func certDue(st store.TLSCertState, ok bool, now time.Time, renewBefore, retryAfter time.Duration) bool {
	if ok && st.HasCert && st.NotAfter != nil && st.NotAfter.Sub(now) >= renewBefore {
		return false
	}
	return !ok || st.LastTryAt == nil || now.Sub(*st.LastTryAt) >= retryAfter
}

// certsOnce - один проход: выпуск недостающих и продление истекающих
// сертификатов доменов включённых релеев, затем публикация tls.web машинам, которым
// он назначен. Только на мастере: на реплике база только для чтения.
func (s *Server) certsOnce(ctx context.Context) {
	a := s.deps.ACME
	if a == nil || s.deps.Certs == nil {
		return
	}
	st, err := s.currentRole(ctx)
	if err != nil || !st.IsMaster() {
		return
	}
	a.defaults()
	targets, err := s.deps.Certs.TLSTargets(ctx)
	if err != nil {
		s.deps.Log.Error("сертификаты: выборка релеев", "err", err)
		return
	}
	var domains []string
	seen := map[string]bool{}
	for _, t := range targets {
		if t.Invalid != "" {
			s.deps.Log.Warn("сертификаты: web_domains машины не разбираются, машина пропущена", "target", t.Code, "err", t.Invalid)
			continue
		}
		for _, d := range t.Domains {
			if !seen[d] {
				seen[d] = true
				domains = append(domains, d)
			}
		}
	}
	states, err := s.deps.Certs.TLSCertStates(ctx, domains)
	if err != nil {
		s.deps.Log.Error("сертификаты: состояние выпуска", "err", err)
		return
	}
	for _, d := range domains {
		if ctx.Err() != nil {
			return
		}
		cur, ok := states[d]
		if !certDue(cur, ok, a.Now(), a.RenewBefore, a.RetryAfter) {
			continue
		}
		s.issueCert(ctx, d)
	}
	for _, t := range targets {
		if t.Invalid != "" || !t.Assigned || len(t.Domains) == 0 {
			continue
		}
		res, err := s.deps.Certs.PublishTLSWeb(ctx, t.Code)
		if err != nil {
			s.deps.Log.Error("сертификаты: публикация tls.web", "target", t.Code, "err", err)
			continue
		}
		if len(res.Changed) > 0 {
			s.deps.Log.Info("сертификаты: новая ревизия tls.web", "target", t.Code, "revision", res.Revision)
			s.revs.bump()
		}
	}
}

// issueCert выпускает сертификат домена; неудача - last_error в базу и WARN в лог.
func (s *Server) issueCert(ctx context.Context, domain string) {
	a := s.deps.ACME
	ictx, cancel := context.WithTimeout(ctx, a.IssueTimeout)
	defer cancel()
	cert, err := a.Issuer.Issue(ictx, domain)
	if err == nil && (len(cert.CertPEM) == 0 || len(cert.KeyPEM) == 0) {
		err = errors.New("выпуск вернул пустой сертификат или ключ")
	}
	if err != nil {
		s.deps.Log.Warn("сертификаты: выпуск не удался, повтор не раньше чем через RetryAfter",
			"domain", domain, "retry_after", a.RetryAfter.String(), "err", err)
		if rerr := s.deps.Certs.RecordTLSError(ctx, domain, err.Error()); rerr != nil {
			s.deps.Log.Error("сертификаты: запись ошибки выпуска", "domain", domain, "err", rerr)
		}
		return
	}
	if err := s.deps.Certs.SaveTLSCert(ctx, domain, cert.CertPEM, cert.KeyPEM, cert.NotAfter); err != nil {
		s.deps.Log.Error("сертификаты: сохранение выпущенного", "domain", domain, "err", err)
		return
	}
	s.deps.Log.Info("сертификаты: выпущен", "domain", domain, "not_after", cert.NotAfter)
}

func (s *Server) certsLoop(ctx context.Context) {
	s.deps.ACME.defaults()
	t := time.NewTicker(s.deps.ACME.Every)
	defer t.Stop()
	for {
		s.certsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// routesCerts - список сертификатов (панель, бот). Только чтение: работает и на реплике.
// Сертификаты и ключи не отдаются никогда - только сроки и ошибки выпуска.
func (s *Server) routesCerts() {
	s.mux.HandleFunc("GET /api/v1/certs", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, ok := s.readPage(w, r)
		if !ok {
			return
		}
		list, total, err := s.deps.Certs.ListCerts(r.Context(), r.URL.Query().Get("machine"), limit, offset)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, pageBody(list, total, limit, offset))
	})
}
