// Package acme - выпуск сертификатов WEB-доменов на мастере (ADR 0004):
// ACME DNS-01 через Cloudflare (lego). Токен Cloudflare есть только у мастера;
// ключ аккаунта ACME хранится в базе (acme_account), чтобы смена мастера не
// плодила аккаунты.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/registration"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// DirectoryProduction - каталог продакшн Let's Encrypt (по умолчанию).
const DirectoryProduction = lego.LEDirectoryProduction

// Cert - выпущенный сертификат домена.
type Cert struct {
	CertPEM  []byte // полная цепочка (fullchain)
	KeyPEM   []byte
	NotAfter time.Time
}

// Issuer выпускает сертификат одного домена. Цикл мастера работает через этот
// интерфейс, тесты подменяют его фейком без сети.
type Issuer interface {
	Issue(ctx context.Context, domain string) (Cert, error)
}

// AccountStore - аккаунт ACME в базе (реализует store.Store).
type AccountStore interface {
	LoadACMEAccount(ctx context.Context, directory string) (store.ACMEAccount, error)
	SaveACMEAccount(ctx context.Context, a store.ACMEAccount) error
}

// Config - настройки выпуска.
type Config struct {
	Email     string // почта аккаунта ACME
	Directory string // URL каталога ACME; пусто - продакшн Let's Encrypt
	CFToken   string // API-токен Cloudflare с правом Zone.DNS:Edit
	Accounts  AccountStore
	Log       *slog.Logger
	// PropagationTimeout - сколько ждать появления TXT-записи; по умолчанию 2 мин.
	PropagationTimeout time.Duration
}

// Lego - Issuer на lego: DNS-01 через Cloudflare, ключи ECDSA P-256.
type Lego struct {
	cfg Config

	mu     sync.Mutex // выпуски по одному: клиент lego и аккаунт общие
	client *lego.Client
}

// NewLego проверяет настройки; сеть не трогает (аккаунт - при первом выпуске).
func NewLego(cfg Config) (*Lego, error) {
	if cfg.Directory == "" {
		cfg.Directory = DirectoryProduction
	}
	switch {
	case !strings.Contains(cfg.Email, "@"):
		return nil, errors.New("acme: нужна почта аккаунта")
	case strings.TrimSpace(cfg.CFToken) == "":
		return nil, errors.New("acme: нужен токен Cloudflare")
	case cfg.Accounts == nil:
		return nil, errors.New("acme: нужно хранилище аккаунта")
	}
	if u, err := url.Parse(cfg.Directory); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("acme: каталог %q - нужен https-адрес", cfg.Directory)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.PropagationTimeout <= 0 {
		cfg.PropagationTimeout = 2 * time.Minute
	}
	return &Lego{cfg: cfg}, nil
}

// user - аккаунт ACME для lego.
type user struct {
	email string
	reg   *registration.Resource
	key   crypto.PrivateKey
}

func (u *user) GetEmail() string                        { return u.email }
func (u *user) GetRegistration() *registration.Resource { return u.reg }
func (u *user) GetPrivateKey() crypto.PrivateKey        { return u.key }

// account - аккаунт каталога из базы; нет - новый ключ, сохранённый в базе до
// регистрации (иначе сбой между регистрацией и записью оставил бы аккаунт без ключа).
func (l *Lego) account(ctx context.Context) (*user, error) {
	acc, err := l.cfg.Accounts.LoadACMEAccount(ctx, l.cfg.Directory)
	if errors.Is(err, store.ErrNoACMEAccount) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("ключ аккаунта ACME: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("ключ аккаунта ACME: %w", err)
		}
		acc = store.ACMEAccount{Directory: l.cfg.Directory, Email: l.cfg.Email,
			KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}
		if err := l.cfg.Accounts.SaveACMEAccount(ctx, acc); err != nil {
			return nil, err
		}
		return &user{email: l.cfg.Email, key: key}, nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(acc.KeyPEM)
	if block == nil {
		return nil, errors.New("ключ аккаунта ACME в базе не читается")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ключ аккаунта ACME в базе: %w", err)
	}
	u := &user{email: l.cfg.Email, key: key}
	// Аккаунт уже зарегистрирован - повторно не регистрируемся (ключ тот же).
	if len(acc.Registration) > 0 {
		var reg registration.Resource
		if err := json.Unmarshal(acc.Registration, &reg); err != nil {
			return nil, fmt.Errorf("регистрация ACME в базе: %w", err)
		}
		u.reg = &reg
	}
	return u, nil
}

// clientLocked - клиент lego с зарегистрированным аккаунтом и провайдером Cloudflare.
func (l *Lego) clientLocked(ctx context.Context) (*lego.Client, error) {
	if l.client != nil {
		return l.client, nil
	}
	u, err := l.account(ctx)
	if err != nil {
		return nil, err
	}
	cfg := lego.NewConfig(u)
	cfg.CADirURL = l.cfg.Directory
	cfg.Certificate.KeyType = certcrypto.EC256
	cfg.UserAgent = "telemux"
	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("клиент ACME: %w", err)
	}
	cf := cloudflare.NewDefaultConfig()
	cf.AuthToken = strings.TrimSpace(l.cfg.CFToken)
	cf.PropagationTimeout = l.cfg.PropagationTimeout
	prov, err := cloudflare.NewDNSProviderConfig(cf)
	if err != nil {
		return nil, fmt.Errorf("провайдер Cloudflare: %w", err)
	}
	if err := client.Challenge.SetDNS01Provider(prov); err != nil {
		return nil, fmt.Errorf("DNS-01: %w", err)
	}
	if u.reg == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("регистрация аккаунта ACME: %w", err)
		}
		raw, err := json.Marshal(reg)
		if err != nil {
			return nil, fmt.Errorf("регистрация аккаунта ACME: %w", err)
		}
		acc, err := l.cfg.Accounts.LoadACMEAccount(ctx, l.cfg.Directory)
		if err != nil {
			return nil, err
		}
		acc.Registration = raw
		if err := l.cfg.Accounts.SaveACMEAccount(ctx, acc); err != nil {
			return nil, err
		}
		u.reg = reg
		l.cfg.Log.Info("acme: аккаунт зарегистрирован", "directory", l.cfg.Directory)
	}
	l.client = client
	return client, nil
}

// Issue выпускает сертификат домена (новый ключ на каждый выпуск).
// lego не принимает контекст: отмена ctx ждёт конца текущего выпуска
// (ограничен таймаутом распространения DNS).
func (l *Lego) Issue(ctx context.Context, domain string) (Cert, error) {
	if !store.ValidDomain(domain) {
		return Cert{}, fmt.Errorf("acme: домен %q не допустим", domain)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Cert{}, err
	}
	client, err := l.clientLocked(ctx)
	if err != nil {
		return Cert{}, err
	}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{domain}, Bundle: true})
	if err != nil {
		return Cert{}, fmt.Errorf("выпуск %s: %w", domain, err)
	}
	na, err := NotAfter(res.Certificate, domain)
	if err != nil {
		return Cert{}, err
	}
	return Cert{CertPEM: res.Certificate, KeyPEM: res.PrivateKey, NotAfter: na}, nil
}

// NotAfter - срок первого (листового) сертификата цепочки; заодно проверяет,
// что он выписан на домен.
func NotAfter(chainPEM []byte, domain string) (time.Time, error) {
	block, _ := pem.Decode(chainPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return time.Time{}, errors.New("сертификат не читается: нет блока CERTIFICATE")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("сертификат не читается: %w", err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return time.Time{}, fmt.Errorf("сертификат не на %s: %w", domain, err)
	}
	return leaf.NotAfter, nil
}
