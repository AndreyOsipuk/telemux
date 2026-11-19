package store

// Сертификаты WEB-доменов релеев (ресурс tls.web, ADR 0004): список доменов
// релея в targets.settings.web_domains, сертификаты и аккаунт ACME - в базе
// мастера (migrations/0009_tls_certs.sql).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// ResourceTLSWeb - сертификаты WEB-доменов релея: JSON {"<домен>": {"cert", "key"}}.
	// Собирает сам мастер из tls_certs, извне не публикуется.
	ResourceTLSWeb = "tls.web"
	// SettingWebDomains - ключ targets.settings релея: домены через запятую.
	SettingWebDomains = "web_domains"
)

// TLSTables - таблицы миграции 0009; без них выпуск сертификатов не запускается.
var TLSTables = []string{"tls_certs", "acme_account"}

var (
	// ErrBadSettings - недопустимые настройки машины (targets.settings).
	ErrBadSettings = errors.New("настройки машины")
	// ErrNoACMEAccount - аккаунта ACME для этого каталога ещё нет.
	ErrNoACMEAccount = errors.New("аккаунта ACME нет")
)

// maxWebDomains - верхний предел доменов на релей (защита от мусора в настройках).
const maxWebDomains = 50

// ValidDomain - имя домена для сертификата: только строчные буквы, цифры и дефис
// в метках, без звёздочек, точки в конце и IP-адресов. Имя потом становится
// именем файла на машине, поэтому проверка строгая.
func ValidDomain(d string) bool {
	if len(d) == 0 || len(d) > 253 {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	// Зона верхнего уровня - не число: так отсекаются IP-адреса.
	tld := labels[len(labels)-1]
	return strings.ContainsFunc(tld, func(c rune) bool { return c >= 'a' && c <= 'z' })
}

// ParseWebDomains разбирает список доменов через запятую: регистр приводится к
// строчному, пробелы и повторы убираются, порядок сохраняется. Пустой - без ошибки.
func ParseWebDomains(s string) ([]string, error) {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		d := strings.ToLower(strings.TrimSpace(part))
		if d == "" || slices.Contains(out, d) {
			continue
		}
		if !ValidDomain(d) {
			return nil, fmt.Errorf("домен %q: допустимы только имена (буквы, цифры, дефис), без звёздочек", strings.TrimSpace(part))
		}
		out = append(out, d)
	}
	if len(out) > maxWebDomains {
		return nil, fmt.Errorf("доменов %d, не больше %d", len(out), maxWebDomains)
	}
	return out, nil
}

// normalizeSettings проверяет настройки машины перед записью и приводит
// web_domains к каноническому виду («a.example.com,b.example.com»).
func normalizeSettings(kind string, settings map[string]string) error {
	raw, ok := settings[SettingWebDomains]
	if !ok {
		return nil
	}
	domains, err := ParseWebDomains(raw)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBadSettings, SettingWebDomains, err)
	}
	if len(domains) == 0 {
		delete(settings, SettingWebDomains)
		return nil
	}
	if kind != "relay" {
		return fmt.Errorf("%w: %s задаются только релею, а машина - %s", ErrBadSettings, SettingWebDomains, kind)
	}
	settings[SettingWebDomains] = strings.Join(domains, ",")
	return nil
}

// TLSWebEntry - сертификат и ключ одного домена в ресурсе tls.web.
type TLSWebEntry struct {
	Cert string `json:"cert"` // полная цепочка PEM
	Key  string `json:"key"`  // закрытый ключ PEM
}

// TLSTarget - включённый релей с доменами для выпуска.
type TLSTarget struct {
	Code     string
	Domains  []string
	Assigned bool   // машине назначен ресурс tls.web
	Invalid  string // web_domains не разбираются (записаны в обход проверки) - причина
}

// TLSCertState - состояние выпуска домена без самих сертификата и ключа.
type TLSCertState struct {
	Domain    string
	HasCert   bool
	NotAfter  *time.Time
	IssuedAt  *time.Time
	LastTryAt *time.Time
	LastError string
}

// TLSTargets - включённые релеи с непустыми web_domains, по коду.
func (s *Store) TLSTargets(ctx context.Context) ([]TLSTarget, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT code, settings->>'web_domains', $1 = ANY(resources) FROM targets
		  WHERE kind = 'relay' AND enabled AND btrim(COALESCE(settings->>'web_domains', '')) <> ''
		  ORDER BY code`, ResourceTLSWeb)
	if err != nil {
		return nil, fmt.Errorf("релеи с доменами: %w", err)
	}
	defer rows.Close()
	out := []TLSTarget{}
	for rows.Next() {
		var t TLSTarget
		var raw string
		if err := rows.Scan(&t.Code, &raw, &t.Assigned); err != nil {
			return nil, fmt.Errorf("scan релея с доменами: %w", err)
		}
		if t.Domains, err = ParseWebDomains(raw); err != nil {
			t.Domains, t.Invalid = []string{}, err.Error()
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TLSCertStates - состояние выпуска доменов одним запросом; домена без строки в
// tls_certs в ответе нет.
func (s *Store) TLSCertStates(ctx context.Context, domains []string) (map[string]TLSCertState, error) {
	out := map[string]TLSCertState{}
	if len(domains) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT domain, cert_pem IS NOT NULL, not_after, issued_at, last_try_at, COALESCE(last_error, '')
		   FROM tls_certs WHERE domain = ANY($1)`, domains)
	if err != nil {
		return nil, fmt.Errorf("состояние сертификатов: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st TLSCertState
		if err := rows.Scan(&st.Domain, &st.HasCert, &st.NotAfter, &st.IssuedAt, &st.LastTryAt, &st.LastError); err != nil {
			return nil, fmt.Errorf("scan сертификата: %w", err)
		}
		out[st.Domain] = st
	}
	return out, rows.Err()
}

// SaveTLSCert сохраняет выпущенный сертификат домена и сбрасывает ошибку.
func (s *Store) SaveTLSCert(ctx context.Context, domain string, certPEM, keyPEM []byte, notAfter time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tls_certs (domain, cert_pem, key_pem, not_after, issued_at, last_error, last_try_at)
		 VALUES ($1, $2, $3, $4, now(), NULL, now())
		 ON CONFLICT (domain) DO UPDATE
		   SET cert_pem = EXCLUDED.cert_pem, key_pem = EXCLUDED.key_pem, not_after = EXCLUDED.not_after,
		       issued_at = now(), last_error = NULL, last_try_at = now()`,
		domain, string(certPEM), string(keyPEM), notAfter)
	if err != nil {
		return fmt.Errorf("сохранение сертификата %s: %w", domain, err)
	}
	return nil
}

// RecordTLSError записывает неудачу выпуска; прежний сертификат остаётся.
func (s *Store) RecordTLSError(ctx context.Context, domain, msg string) error {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tls_certs (domain, last_error, last_try_at) VALUES ($1, $2, now())
		 ON CONFLICT (domain) DO UPDATE SET last_error = EXCLUDED.last_error, last_try_at = now()`,
		domain, strings.ToValidUTF8(msg, "?"))
	if err != nil {
		return fmt.Errorf("ошибка выпуска %s: %w", domain, err)
	}
	return nil
}

// PublishTLSWeb собирает ресурс tls.web машины из сертификатов её доменов и
// публикует его, если содержимое изменилось. Доменов с сертификатом нет -
// публиковать нечего (Changed пуст). Машине без tls.web - ErrResourceNotAssigned.
// Только на primary.
func (s *Store) PublishTLSWeb(ctx context.Context, target string) (PublishResult, error) {
	res := PublishResult{Changed: []string{}}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin publish tls.web: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, publishLockKey); err != nil {
		return res, fmt.Errorf("publish lock: %w", err)
	}
	var raw string
	var assigned bool
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(settings->>'web_domains', ''), $2 = ANY(resources) FROM targets WHERE code = $1 AND enabled`,
		target, ResourceTLSWeb).Scan(&raw, &assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, ErrUnknownTarget
	}
	if err != nil {
		return res, fmt.Errorf("target %q: %w", target, err)
	}
	if !assigned {
		return res, ErrResourceNotAssigned
	}
	domains, err := ParseWebDomains(raw)
	if err != nil {
		return res, fmt.Errorf("%w: машина %s: %v", ErrBadSettings, target, err)
	}
	content := map[string]TLSWebEntry{}
	if len(domains) > 0 {
		rows, err := tx.Query(ctx,
			`SELECT domain, cert_pem, key_pem FROM tls_certs WHERE domain = ANY($1) AND cert_pem IS NOT NULL`, domains)
		if err != nil {
			return res, fmt.Errorf("сертификаты машины %s: %w", target, err)
		}
		for rows.Next() {
			var d string
			var e TLSWebEntry
			if err := rows.Scan(&d, &e.Cert, &e.Key); err != nil {
				rows.Close()
				return res, fmt.Errorf("scan сертификата: %w", err)
			}
			content[d] = e
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, fmt.Errorf("сертификаты машины %s: %w", target, err)
		}
	}
	if len(content) == 0 {
		return res, nil
	}
	// encoding/json пишет ключи словаря по порядку - одинаковое содержимое даёт тот же хеш.
	body, err := json.Marshal(content)
	if err != nil {
		return res, fmt.Errorf("tls.web машины %s: %w", target, err)
	}
	if res, err = publishLocked(ctx, tx, target, ResourceTLSWeb, body); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit publish tls.web: %w", err)
	}
	return res, nil
}

// CertInfo - строка списка сертификатов: релей, домен и состояние выпуска.
// Сертификата и ключа здесь нет и быть не должно.
type CertInfo struct {
	Machine   string     `json:"machine"`
	Domain    string     `json:"domain"`
	Assigned  bool       `json:"assigned"` // машине назначен tls.web
	NotAfter  *time.Time `json:"not_after"`
	IssuedAt  *time.Time `json:"issued_at"`
	LastError string     `json:"last_error,omitempty"`
	LastTryAt *time.Time `json:"last_try_at"`
}

// certDomainsCTE - пары (включённый релей, домен) из web_domains.
const certDomainsCTE = `WITH d AS (
  SELECT t.code, $1 = ANY(t.resources) AS assigned, lower(btrim(x)) AS domain
    FROM targets t, unnest(string_to_array(t.settings->>'web_domains', ',')) AS x
   WHERE t.kind = 'relay' AND t.enabled AND btrim(x) <> '')`

// ListCerts - домены включённых релеев и состояние их сертификатов по машине и
// домену, постранично; machine не пустой - только эта машина; total - всего строк
// с фильтром.
func (s *Store) ListCerts(ctx context.Context, machine string, limit, offset int) ([]CertInfo, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, certDomainsCTE+` SELECT count(*) FROM d WHERE ($2 = '' OR d.code = $2)`,
		ResourceTLSWeb, machine).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count certs: %w", err)
	}
	rows, err := s.pool.Query(ctx, certDomainsCTE+`
		SELECT d.code, d.domain, d.assigned, c.not_after, c.issued_at, COALESCE(c.last_error, ''), c.last_try_at
		  FROM d LEFT JOIN tls_certs c ON c.domain = d.domain
		 WHERE ($2 = '' OR d.code = $2)
		 ORDER BY d.code, d.domain LIMIT $3 OFFSET $4`, ResourceTLSWeb, machine, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("certs: %w", err)
	}
	defer rows.Close()
	out := []CertInfo{}
	for rows.Next() {
		var c CertInfo
		if err := rows.Scan(&c.Machine, &c.Domain, &c.Assigned, &c.NotAfter, &c.IssuedAt, &c.LastError, &c.LastTryAt); err != nil {
			return nil, 0, fmt.Errorf("scan cert: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("certs: %w", err)
	}
	return out, total, nil
}

// CertCounts - итоги по сертификатам доменов включённых релеев (строки как в ListCerts).
type CertCounts struct {
	Total    int // строк (машина, домен)
	Expiring int // сертификат есть и кончается раньше before (в том числе уже истёк)
	Missing  int // сертификата ещё нет
}

// CertCounts считает итоги одним запросом; before - граница «скоро истекает».
func (s *Store) CertCounts(ctx context.Context, before time.Time) (CertCounts, error) {
	var c CertCounts
	err := s.pool.QueryRow(ctx, certDomainsCTE+`
		SELECT count(*)::int,
		       count(*) FILTER (WHERE c.not_after IS NOT NULL AND c.not_after < $2)::int,
		       count(*) FILTER (WHERE c.not_after IS NULL)::int
		  FROM d LEFT JOIN tls_certs c ON c.domain = d.domain`, ResourceTLSWeb, before).
		Scan(&c.Total, &c.Expiring, &c.Missing)
	if err != nil {
		return c, fmt.Errorf("итоги сертификатов: %w", err)
	}
	return c, nil
}

// ACMEAccount - аккаунт ACME мастера для одного каталога.
type ACMEAccount struct {
	Directory    string
	Email        string
	KeyPEM       []byte // PKCS#8
	Registration []byte // JSON ответа регистрации; nil - ещё не зарегистрирован
}

// LoadACMEAccount - аккаунт каталога; нет - ErrNoACMEAccount.
func (s *Store) LoadACMEAccount(ctx context.Context, directory string) (ACMEAccount, error) {
	a := ACMEAccount{Directory: directory}
	var key string
	err := s.pool.QueryRow(ctx,
		`SELECT email, key_pem, registration FROM acme_account WHERE directory = $1`, directory).
		Scan(&a.Email, &key, &a.Registration)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNoACMEAccount
	}
	if err != nil {
		return a, fmt.Errorf("аккаунт ACME: %w", err)
	}
	a.KeyPEM = []byte(key)
	return a, nil
}

// SaveACMEAccount создаёт или обновляет аккаунт каталога.
func (s *Store) SaveACMEAccount(ctx context.Context, a ACMEAccount) error {
	var reg any
	if len(a.Registration) > 0 {
		reg = string(a.Registration)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO acme_account (directory, email, key_pem, registration) VALUES ($1, $2, $3, $4::jsonb)
		 ON CONFLICT (directory) DO UPDATE
		   SET email = EXCLUDED.email, key_pem = EXCLUDED.key_pem, registration = EXCLUDED.registration, updated_at = now()`,
		a.Directory, a.Email, string(a.KeyPEM), reg)
	if err != nil {
		return fmt.Errorf("сохранение аккаунта ACME: %w", err)
	}
	return nil
}

// CheckTLSSchema - применена ли миграция 0009 (таблицы сертификатов).
func (s *Store) CheckTLSSchema(ctx context.Context) error {
	return s.checkTables(ctx, TLSTables, "migrations/0009_tls_certs.sql")
}
