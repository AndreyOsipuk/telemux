package agent

// Применение сертификатов WEB-доменов (ресурс tls.web, ADR 0004). Выпускает и
// продлевает мастер; агент релея раскладывает файлы для nginx:
// <каталог>/<домен>.pem (полная цепочка) и <каталог>/<домен>.key (0600).
//
// Порядок: разбор и проверки (имя домена, пара сертификат-ключ, сертификат на
// этот домен) -> место -> для изменившихся доменов временные файлы со сверкой ->
// бэкап прежних -> замена -> nginx -t -> systemctl reload nginx. Провал nginx -t
// или перезагрузки - возврат прежних файлов (новые домены удаляются).
// Домены, которых нет в ресурсе, агент не трогает и не удаляет: в каталоге могут
// лежать сертификаты, выпущенные иначе.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// DefaultTLSDir - каталог сертификатов nginx по умолчанию.
const DefaultTLSDir = "/etc/nginx/certs"

// NginxService - проверка конфига и перезагрузка nginx.
type NginxService interface {
	Test(ctx context.Context) error   // nginx -t
	Reload(ctx context.Context) error // перечитать конфиг и сертификаты
}

// SystemdNginx - nginx под systemd.
type SystemdNginx struct {
	Unit string // юнит; по умолчанию nginx
	Bin  string // бинарь для nginx -t; по умолчанию nginx
}

// Test - nginx -t; в ошибке - вывод nginx.
func (n SystemdNginx) Test(ctx context.Context) error {
	bin := n.Bin
	if bin == "" {
		bin = "nginx"
	}
	return runCmd(ctx, bin, "-t")
}

// Reload - systemctl reload юнита.
func (n SystemdNginx) Reload(ctx context.Context) error {
	unit := n.Unit
	if unit == "" {
		unit = "nginx"
	}
	return runCmd(ctx, "systemctl", "reload", unit)
}

var _ NginxService = SystemdNginx{}

// TLSApplier раскладывает сертификаты tls.web.
type TLSApplier struct {
	Dir       string       // каталог сертификатов; по умолчанию DefaultTLSDir
	Nginx     NginxService // по умолчанию SystemdNginx{}
	FreeSpace func(dir string) (uint64, error)

	writeFile func(path string, data []byte, perm os.FileMode) error
}

func (a *TLSApplier) defaults() {
	if a.Dir == "" {
		a.Dir = DefaultTLSDir
	}
	if a.Nginx == nil {
		a.Nginx = SystemdNginx{}
	}
	if a.FreeSpace == nil {
		a.FreeSpace = freeSpace
	}
	if a.writeFile == nil {
		a.writeFile = writeFileSync
	}
}

// tlsFile - один файл домена: путь, желаемое содержимое, права.
type tlsFile struct {
	path string
	data []byte
	perm os.FileMode
}

func domainFiles(dir, domain string, e store.TLSWebEntry) []tlsFile {
	return []tlsFile{
		{path: filepath.Join(dir, domain+".pem"), data: []byte(e.Cert), perm: 0o644},
		{path: filepath.Join(dir, domain+".key"), data: []byte(e.Key), perm: 0o600},
	}
}

// parseTLSWeb разбирает ресурс и проверяет каждый домен до любой записи на диск.
func parseTLSWeb(content []byte) (map[string]store.TLSWebEntry, []string, error) {
	var m map[string]store.TLSWebEntry
	if err := json.Unmarshal(content, &m); err != nil {
		return nil, nil, fmt.Errorf("tls.web не читается: %w", err)
	}
	domains := make([]string, 0, len(m))
	for d, e := range m {
		// Имя домена становится именем файла: только допустимые имена, без путей и звёздочек.
		if !store.ValidDomain(d) {
			return nil, nil, fmt.Errorf("домен %q в tls.web не допустим", d)
		}
		pair, err := tls.X509KeyPair([]byte(e.Cert), []byte(e.Key))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: сертификат и ключ не составляют пару: %v", d, err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, nil, fmt.Errorf("%s: сертификат не читается: %v", d, err)
		}
		if err := leaf.VerifyHostname(d); err != nil {
			return nil, nil, fmt.Errorf("%s: сертификат выписан не на этот домен: %v", d, err)
		}
		domains = append(domains, d)
	}
	sort.Strings(domains)
	return m, domains, nil
}

// Apply приводит файлы доменов ресурса к желаемым и перезагружает nginx.
func (a *TLSApplier) Apply(ctx context.Context, content []byte) ApplyResult {
	a.defaults()
	entries, domains, err := parseTLSWeb(content)
	if err != nil {
		return failed("%v", err)
	}
	// Что менять: файлы, которые отличаются от желаемых (или отличаются права ключа).
	var changed []tlsFile
	need := uint64(1 << 20)
	for _, d := range domains {
		for _, f := range domainFiles(a.Dir, d, entries[d]) {
			cur, err := os.ReadFile(f.path)
			if err != nil && !os.IsNotExist(err) {
				return failed("%s не читается: %v", f.path, err)
			}
			if err == nil && bytes.Equal(cur, f.data) && modeOK(f) {
				continue
			}
			changed = append(changed, f)
			need += uint64(2 * len(f.data))
		}
	}
	if len(changed) == 0 {
		return ApplyResult{Status: "applied"}
	}
	if err := os.MkdirAll(a.Dir, 0o755); err != nil {
		return failed("каталог сертификатов %s: %v (у юнита агента он должен быть в ReadWritePaths)", a.Dir, err)
	}
	if free, err := a.FreeSpace(a.Dir); err != nil {
		return failed("свободное место не проверить: %v", err)
	} else if free < need {
		return failed("мало места на диске: свободно %d байт, нужно %d", free, need)
	}

	// Временные файлы со сверкой - до замены чего-либо.
	cleanup := func() {
		for _, f := range changed {
			_ = os.Remove(f.path + tmpSuffix)
		}
	}
	for _, f := range changed {
		tmp := f.path + tmpSuffix
		_ = os.Remove(tmp) // прежний мусор с другими правами: O_CREATE их не поменяет
		if err := a.writeFile(tmp, f.data, f.perm); err != nil {
			cleanup()
			return failed("запись %s: %v", tmp, err)
		}
		if err := os.Chmod(tmp, f.perm); err != nil {
			cleanup()
			return failed("права %s: %v", tmp, err)
		}
		if got, err := os.ReadFile(tmp); err != nil || !bytes.Equal(got, f.data) {
			cleanup()
			return failed("записанный %s обрезан или не совпал с желаемым (%d из %d байт)", tmp, len(got), len(f.data))
		}
	}

	// Бэкап прежних и замена; replaced - что откатывать.
	type replacedFile struct {
		path   string
		hadOld bool
	}
	var replaced []replacedFile
	rollback := func() []string {
		var errs []string
		for i := len(replaced) - 1; i >= 0; i-- {
			r := replaced[i]
			var err error
			if r.hadOld {
				err = os.Rename(r.path+bakSuffix, r.path)
			} else {
				err = os.Remove(r.path)
			}
			if err != nil && !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
		}
		cleanup()
		return errs
	}
	for _, f := range changed {
		cur, err := os.ReadFile(f.path)
		hadOld := err == nil
		if err != nil && !os.IsNotExist(err) {
			rollback()
			return failed("%s не читается: %v", f.path, err)
		}
		if hadOld {
			bak := f.path + bakSuffix
			_ = os.Remove(bak)
			// Бэкап с правами нового файла: бэкап ключа - тоже 0600.
			if err := a.writeFile(bak, cur, f.perm); err != nil {
				rollback()
				return failed("бэкап %s: %v", f.path, err)
			}
			if err := os.Chmod(bak, f.perm); err != nil {
				rollback()
				return failed("права бэкапа %s: %v", f.path, err)
			}
		}
		if err := os.Rename(f.path+tmpSuffix, f.path); err != nil {
			rollback()
			return failed("замена %s: %v", f.path, err)
		}
		replaced = append(replaced, replacedFile{path: f.path, hadOld: hadOld})
	}

	undo := func(cause string) ApplyResult {
		if errs := rollback(); len(errs) > 0 {
			return failed("%s; возврат прежних файлов не удался: %s", cause, strings.Join(errs, "; "))
		}
		return failed("%s; прежние файлы возвращены", cause)
	}
	if err := a.Nginx.Test(ctx); err != nil {
		return undo(fmt.Sprintf("nginx -t не принял новые сертификаты: %v", err))
	}
	if err := a.Nginx.Reload(ctx); err != nil {
		// nginx при неудачной перезагрузке работает на прежней конфигурации; файлы - тоже прежние.
		return undo(fmt.Sprintf("перезагрузка nginx не удалась: %v", err))
	}
	return ApplyResult{Status: "applied"}
}

// modeOK - права файла на диске совпадают с нужными (для ключа - строго 0600).
func modeOK(f tlsFile) bool {
	fi, err := os.Stat(f.path)
	return err == nil && fi.Mode().Perm() == f.perm
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// CompareTLSWeb - какие домены ресурса отличаются от файлов на машине (по sha256
// сертификата и ключа). Пустая строка - всё совпадает. Ничего не пишет.
func CompareTLSWeb(dir string, content []byte) (string, error) {
	entries, domains, err := parseTLSWeb(content)
	if err != nil {
		return "", err
	}
	var missing, differ []string
	for _, d := range domains {
		e := entries[d]
		cert, err := os.ReadFile(filepath.Join(dir, d+".pem"))
		switch {
		case os.IsNotExist(err):
			missing = append(missing, d)
			continue
		case err != nil:
			return "", fmt.Errorf("%s.pem не читается: %w", d, err)
		}
		key, kerr := os.ReadFile(filepath.Join(dir, d+".key"))
		if kerr != nil && !os.IsNotExist(kerr) {
			return "", fmt.Errorf("%s.key не читается: %w", d, kerr)
		}
		if sha256hex(cert) != sha256hex([]byte(e.Cert)) || kerr != nil || sha256hex(key) != sha256hex([]byte(e.Key)) {
			differ = append(differ, d)
		}
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("нет на машине: %d (%s)", len(missing), strings.Join(missing, ", ")))
	}
	if len(differ) > 0 {
		parts = append(parts, fmt.Sprintf("отличаются: %d (%s)", len(differ), strings.Join(differ, ", ")))
	}
	return strings.Join(parts, "; "), nil
}

// compareTLSWeb - отчёт режима сравнения по ресурсу tls.web.
func (a *Agent) compareTLSWeb(d *store.Desired) store.ObservedResource {
	res := store.ObservedResource{Resource: d.Resource, Revision: d.Revision, SHA256: d.SHA256, Status: "compared"}
	diff, err := CompareTLSWeb(a.cfg.TLSDir, d.Content)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	res.Error = diff
	return res
}

// errNoTLSApplier - для apply с tls.web нужен TLSApplier.
var errNoTLSApplier = errors.New("для режима apply с tls.web нужен TLSApplier")
