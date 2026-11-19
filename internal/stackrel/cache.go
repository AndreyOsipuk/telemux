package stackrel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/AndreyOsipuk/telemux/internal/stack"
)

// DefaultCacheDir - каталог кэша бинарей на мастере по умолчанию.
const DefaultCacheDir = "/var/lib/telemux/stack"

// Cache - бинари на диске мастера: файл с именем = SHA256 бинаря.
type Cache struct {
	Dir string
}

// Path - путь к бинарю; sha256 не по формату - ошибка (защита от выхода из каталога).
func (c Cache) Path(sha string) (string, error) {
	if !stack.ValidSHA256(sha) {
		return "", fmt.Errorf("sha256 %q: нужно 64 строчных hex", sha)
	}
	return filepath.Join(c.Dir, sha), nil
}

// Put кладёт бинарь в кэш атомарно (временный файл в том же каталоге и rename) и
// возвращает его SHA256 и размер. Файл перезаписывается всегда: лежащему с тем же именем
// не доверяем (мог быть испорчен).
func (c Cache) Put(data []byte) (sha string, size int64, err error) {
	if len(data) == 0 {
		return "", 0, errors.New("кэш: пустой бинарь")
	}
	sha = SHA256Hex(data)
	p, err := c.Path(sha)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(c.Dir, 0o750); err != nil {
		return "", 0, fmt.Errorf("кэш: каталог %s: %w", c.Dir, err)
	}
	tmp, err := os.CreateTemp(c.Dir, ".tmp-"+sha[:12]+"-*")
	if err != nil {
		return "", 0, fmt.Errorf("кэш: временный файл: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // после rename файла уже нет
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return "", 0, fmt.Errorf("кэш: запись: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return "", 0, fmt.Errorf("кэш: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("кэш: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return "", 0, fmt.Errorf("кэш: chmod: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return "", 0, fmt.Errorf("кэш: rename: %w", err)
	}
	return sha, int64(len(data)), nil
}

// Verify - лежит ли в кэше бинарь sha256 и совпадает ли хэш его содержимого.
func (c Cache) Verify(sha string) bool {
	f, _, err := c.Open(sha)
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == sha
}

// Open открывает бинарь из кэша. Нет файла - ошибка с fs.ErrNotExist.
func (c Cache) Open(sha string) (*os.File, int64, error) {
	p, err := c.Path(sha)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", fs.ErrNotExist, err)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck
		return nil, 0, err
	}
	if !st.Mode().IsRegular() {
		f.Close() //nolint:errcheck
		return nil, 0, fmt.Errorf("%s: не обычный файл: %w", p, fs.ErrNotExist)
	}
	return f, st.Size(), nil
}
