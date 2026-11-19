// Package tlspin - TLS с закреплённым сертификатом: клиент доверяет ровно одному
// сертификату по его sha256-отпечатку, без центров сертификации. Используется
// между машинами флота и мастером и с мастер-панелью 3x-ui.
package tlspin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"
)

// Normalize приводит отпечаток к виду hex без двоеточий в нижнем регистре.
func Normalize(fp string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
}

// Fingerprint - sha256 сертификата в DER.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Config - клиентский TLS, который принимает только сертификат с этим отпечатком.
// Отпечаток проверяется до отправки запроса, поэтому токен чужому серверу не уходит.
func Config(fingerprint string) *tls.Config {
	want := Normalize(fingerprint)
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // проверка - по отпечатку в VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("сервер не предъявил сертификат")
			}
			if Fingerprint(cs.PeerCertificates[0].Raw) != want {
				return errors.New("отпечаток сертификата сервера не совпал с закреплённым")
			}
			return nil
		},
	}
}

// Generate выпускает самоподписанный сертификат ECDSA P-256 на 10 лет и пишет
// cert (0644) и key (0600). Возвращает отпечаток для закрепления у клиентов.
func Generate(certFile, keyFile, commonName string) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("ключ: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return "", fmt.Errorf("сертификат: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", fmt.Errorf("запись ключа: %w", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", fmt.Errorf("запись сертификата: %w", err)
	}
	return Fingerprint(der), nil
}

// FingerprintFile - отпечаток сертификата из PEM-файла.
func FingerprintFile(certFile string) (string, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s: нет PEM-сертификата", certFile)
	}
	return Fingerprint(block.Bytes), nil
}

// SPKIPinFile - sha256 открытого ключа (SubjectPublicKeyInfo) сертификата из
// PEM-файла в base64: формат curl --pinnedpubkey 'sha256//<значение>'.
func SPKIPinFile(certFile string) (string, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s: нет PEM-сертификата", certFile)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("%s: %w", certFile, err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}
