package mtgmulti

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/backend"
	"golang.org/x/crypto/ssh"
)

// SSHDelivery — доставка config на mtg-ноду по SSH. Читает текущий config,
// атомарно перезаписывает (tmp+mv) и рестартит демон одной сессией.
//
// Тестами не покрыта (реальный I/O); Sync-логика, которая её использует, покрыта
// через mockDelivery. HostKey пока InsecureIgnoreHostKey — для прода заменить на
// known_hosts (TODO), сейчас ноды в доверенной сети + ключевая авторизация.
type SSHDelivery struct {
	KeyPath     string        // путь к приватному ключу
	DialTimeout time.Duration // таймаут соединения (0 → 10s)
}

// NewSSHDelivery создаёт доставку с ключом keyPath.
func NewSSHDelivery(keyPath string) *SSHDelivery {
	return &SSHDelivery{KeyPath: keyPath, DialTimeout: 10 * time.Second}
}

func (d *SSHDelivery) client(node backend.Node) (*ssh.Client, error) {
	keyBytes, err := os.ReadFile(d.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh: чтение ключа %q: %w", d.KeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("ssh: парсинг ключа: %w", err)
	}
	user := node.SSHUser
	if user == "" {
		user = "root"
	}
	port := node.SSHPort
	if port == 0 {
		port = 22
	}
	to := d.DialTimeout
	if to == 0 {
		to = 10 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: known_hosts для прода
		Timeout:         to,
	}
	addr := net.JoinHostPort(node.Address, strconv.Itoa(port))
	return ssh.Dial("tcp", addr, cfg)
}

// ReadConfig читает config.toml с ноды. Отсутствие файла → "" без ошибки.
func (d *SSHDelivery) ReadConfig(ctx context.Context, node backend.Node) (string, error) {
	if node.Mtg == nil || node.Mtg.ConfigPath == "" {
		return "", fmt.Errorf("ssh: не задан ConfigPath ноды %q", node.Code)
	}
	cl, err := d.client(node)
	if err != nil {
		return "", err
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	// `cat файл 2>/dev/null || true` — нет файла → пусто, не ошибка.
	out, err := sess.Output("cat " + shellQuote(node.Mtg.ConfigPath) + " 2>/dev/null || true")
	if err != nil {
		return "", fmt.Errorf("ssh: cat config на %q: %w", node.Code, err)
	}
	return string(out), nil
}

// WriteAndReload атомарно пишет config (tmp+mv) и рестартит демон — одной сессией.
func (d *SSHDelivery) WriteAndReload(ctx context.Context, node backend.Node, config string) error {
	if node.Mtg == nil || node.Mtg.ConfigPath == "" {
		return fmt.Errorf("ssh: не задан ConfigPath ноды %q", node.Code)
	}
	reload := node.Mtg.ReloadCmd
	if reload == "" {
		reload = "systemctl restart mtg-multi"
	}
	cl, err := d.client(node)
	if err != nil {
		return err
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	path := node.Mtg.ConfigPath
	tmp := path + ".tmp"
	// config подаётся в stdin → пишется в tmp → атомарный mv → рестарт.
	// `cat > tmp` берёт ровно stdin, без интерполяции (секреты безопасны).
	script := fmt.Sprintf(
		"set -e; mkdir -p %s; cat > %s; mv %s %s; %s",
		shellQuote(dirOf(path)), shellQuote(tmp), shellQuote(tmp), shellQuote(path), reload,
	)
	sess.Stdin = strings.NewReader(config)
	if out, err := sess.CombinedOutput(script); err != nil {
		return fmt.Errorf("ssh: write+reload на %q: %w (вывод: %s)", node.Code, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// shellQuote — безопасное одинарное квотирование для shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// dirOf возвращает директорию пути (без import path/filepath ради ясности).
func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}
