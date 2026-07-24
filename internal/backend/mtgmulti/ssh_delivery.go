package mtgmulti

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/backend"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHDelivery — доставка config на mtg-ноду по SSH. Читает текущий config,
// атомарно перезаписывает (tmp+mv) и рестартит демон одной сессией.
type SSHDelivery struct {
	signer          ssh.Signer
	hostKeyCallback ssh.HostKeyCallback
	DialTimeout     time.Duration // таймаут соединения (0 → 10s)
}

var systemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9_.@:-]+$`)

// NewSSHDelivery создаёт доставку с ключом keyPath и обязательной проверкой
// host key по OpenSSH known_hosts.
func NewSSHDelivery(keyPath, knownHostsPath string) (*SSHDelivery, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh: чтение ключа %q: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("ssh: парсинг ключа: %w", err)
	}
	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("ssh: known_hosts %q: %w", knownHostsPath, err)
	}
	return &SSHDelivery{
		signer:          signer,
		hostKeyCallback: hostKeyCallback,
		DialTimeout:     10 * time.Second,
	}, nil
}

func (d *SSHDelivery) client(ctx context.Context, node backend.Node) (*ssh.Client, error) {
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
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.signer)},
		HostKeyCallback: d.hostKeyCallback,
		Timeout:         to,
	}
	addr := net.JoinHostPort(node.Address, strconv.Itoa(port))
	raw, err := (&net.Dialer{Timeout: to}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh: dial %s: %w", addr, err)
	}

	deadline := time.Now().Add(to)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := raw.SetDeadline(deadline); err != nil {
		raw.Close()
		return nil, fmt.Errorf("ssh: deadline %s: %w", addr, err)
	}

	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("ssh: handshake %s: %w", addr, err)
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh: clear deadline %s: %w", addr, err)
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

// ReadConfig читает config.toml с ноды. Отсутствие файла → "" без ошибки.
func (d *SSHDelivery) ReadConfig(ctx context.Context, node backend.Node) (string, error) {
	if node.Mtg == nil || node.Mtg.ConfigPath == "" {
		return "", fmt.Errorf("ssh: не задан ConfigPath ноды %q", node.Code)
	}
	cl, err := d.client(ctx, node)
	if err != nil {
		return "", err
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	path := shellQuote(node.Mtg.ConfigPath)
	// Отсутствующий config допустим для новой ноды. Любая другая ошибка чтения
	// должна дойти до Sync, иначе transient SSH/permission failure вызовет
	// ошибочную перезапись и рестарт.
	out, err := outputContext(ctx, sess, "if [ ! -e "+path+" ]; then exit 0; fi; cat "+path)
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
	service := node.Mtg.ServiceName
	if service == "" {
		service = "mtg-multi"
	}
	if !systemdUnitPattern.MatchString(service) {
		return fmt.Errorf("ssh: недопустимое имя systemd-сервиса %q", service)
	}
	cl, err := d.client(ctx, node)
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
	backup := path + ".telemux-backup"
	// Config подаётся в stdin, ставится с mode 0600 и заменяется атомарно. При
	// неуспешном restart восстанавливаем предыдущую версию и пытаемся поднять её.
	script := fmt.Sprintf(
		"set -e; umask 077; mkdir -p %s; cat > %s; chmod 600 %s; "+
			"had_old=0; if [ -e %s ]; then cp -p %s %s; had_old=1; fi; "+
			"mv %s %s; "+
			"if systemctl restart -- %s; then rm -f %s; exit 0; fi; "+
			"if [ \"$had_old\" -eq 1 ]; then mv %s %s; systemctl restart -- %s || true; else rm -f %s; fi; exit 1",
		shellQuote(dirOf(path)),
		shellQuote(tmp),
		shellQuote(tmp),
		shellQuote(path),
		shellQuote(path),
		shellQuote(backup),
		shellQuote(tmp),
		shellQuote(path),
		shellQuote(service),
		shellQuote(backup),
		shellQuote(backup),
		shellQuote(path),
		shellQuote(service),
		shellQuote(path),
	)
	sess.Stdin = strings.NewReader(config)
	if out, err := combinedOutputContext(ctx, sess, script); err != nil {
		return fmt.Errorf("ssh: write+reload на %q: %w (вывод: %s)", node.Code, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type sessionResult struct {
	output []byte
	err    error
}

func outputContext(ctx context.Context, sess *ssh.Session, command string) ([]byte, error) {
	result := make(chan sessionResult, 1)
	go func() {
		output, err := sess.Output(command)
		result <- sessionResult{output: output, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = sess.Close()
		return nil, ctx.Err()
	case r := <-result:
		return r.output, r.err
	}
}

func combinedOutputContext(ctx context.Context, sess *ssh.Session, command string) ([]byte, error) {
	result := make(chan sessionResult, 1)
	go func() {
		output, err := sess.CombinedOutput(command)
		result <- sessionResult{output: output, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = sess.Close()
		return nil, ctx.Err()
	case r := <-result:
		return r.output, r.err
	}
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
