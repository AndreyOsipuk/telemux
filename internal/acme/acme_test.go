package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

type fakeAccounts struct {
	mu    sync.Mutex
	m     map[string]store.ACMEAccount
	saves int
}

func (f *fakeAccounts) LoadACMEAccount(_ context.Context, dir string) (store.ACMEAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.m[dir]
	if !ok {
		return a, store.ErrNoACMEAccount
	}
	return a, nil
}

func (f *fakeAccounts) SaveACMEAccount(_ context.Context, a store.ACMEAccount) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	f.m[a.Directory] = a
	return nil
}

const testDir = "https://acme.example.net/directory"

// Ключ аккаунта создаётся один раз и хранится в базе; новый экземпляр берёт тот же.
func TestAccountKeyPersisted(t *testing.T) {
	accs := &fakeAccounts{m: map[string]store.ACMEAccount{}}
	cfg := Config{Email: "ops@example.com", Directory: testDir, CFToken: "tok", Accounts: accs}
	l1, err := NewLego(cfg)
	if err != nil {
		t.Fatal(err)
	}
	u1, err := l1.account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accs.saves != 1 || len(accs.m[testDir].KeyPEM) == 0 {
		t.Fatalf("ключ аккаунта не сохранён: %+v", accs.m)
	}
	l2, _ := NewLego(cfg)
	u2, err := l2.account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	k1 := u1.key.(*ecdsa.PrivateKey)
	k2 := u2.key.(*ecdsa.PrivateKey)
	if !k1.Equal(k2) || accs.saves != 1 {
		t.Fatalf("второй экземпляр создал новый ключ (сохранений %d)", accs.saves)
	}
	if u2.GetEmail() != "ops@example.com" || u2.GetRegistration() != nil {
		t.Fatalf("аккаунт без регистрации: %+v", u2)
	}
}

func TestNewLego_Validates(t *testing.T) {
	accs := &fakeAccounts{m: map[string]store.ACMEAccount{}}
	for name, cfg := range map[string]Config{
		"без почты":    {Directory: testDir, CFToken: "tok", Accounts: accs},
		"без токена":   {Email: "ops@example.com", Directory: testDir, Accounts: accs},
		"без базы":     {Email: "ops@example.com", Directory: testDir, CFToken: "tok"},
		"плохой URL":   {Email: "ops@example.com", Directory: "acme", CFToken: "tok", Accounts: accs},
		"http-каталог": {Email: "ops@example.com", Directory: "http://acme.example.net/dir", CFToken: "tok", Accounts: accs},
	} {
		if _, err := NewLego(cfg); err == nil {
			t.Fatalf("%s: конфиг принят", name)
		}
	}
	l, err := NewLego(Config{Email: "ops@example.com", CFToken: "tok", Accounts: accs})
	if err != nil || l.cfg.Directory != DirectoryProduction {
		t.Fatalf("по умолчанию - продакшн Let's Encrypt: %v", err)
	}
}

// Срок сертификата берётся из первого (листового) сертификата цепочки.
func TestNotAfter(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	want := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "web1.example.com"},
		DNSNames: []string{"web1.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: want}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	got, err := NotAfter(append(leaf, leaf...), "web1.example.com")
	if err != nil || !got.Equal(want) {
		t.Fatalf("ждали %v, получили %v, %v", want, got, err)
	}
	if _, err := NotAfter(leaf, "web5.example.com"); err == nil {
		t.Fatal("сертификат не на тот домен принят")
	}
	if _, err := NotAfter([]byte("мусор"), "web1.example.com"); err == nil {
		t.Fatal("мусор принят")
	}
}
