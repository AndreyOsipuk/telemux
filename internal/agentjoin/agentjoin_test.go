package agentjoin

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	tok  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	spki = "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	hexp = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestParseExitAddress(t *testing.T) {
	for _, ok := range []string{"203.0.113.10", "10.0.0.1", "1.2.3.4"} {
		if _, err := ParseExitAddress(ok); err != nil {
			t.Errorf("%q: ждали ок, получили %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1.2.3.4;rm", "1.2.3.4 ", " 1.2.3.4", "01.2.3.4", "1.2.3", "::1", "2001:db8::1",
		"::ffff:1.2.3.4", "0.0.0.0", "127.0.0.1", "224.0.0.1", "169.254.1.1", "255.255.255.255", "host.example", "$(id)"} {
		if _, err := ParseExitAddress(bad); err == nil {
			t.Errorf("%q: ждали ошибку", bad)
		}
	}
}

func TestValidBridgeSocks(t *testing.T) {
	for _, ok := range []string{"socks5://127.0.0.1:1", "socks5://127.0.0.1:10872", "socks5://127.0.0.1:65535"} {
		if err := ValidBridgeSocks(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "socks5://127.0.0.1:0", "socks5://127.0.0.1:65536", "socks5://127.0.0.1:010",
		"socks5://10.0.0.1:1080", "socks5h://127.0.0.1:1080", "http://127.0.0.1:1080", "socks5://127.0.0.1:1080/x",
		"socks5://user:pw@127.0.0.1:1080", "socks5://127.0.0.1:1080;id"} {
		if err := ValidBridgeSocks(bad); err == nil {
			t.Errorf("%q: ждали ошибку", bad)
		}
	}
}

func TestValidPublicURL(t *testing.T) {
	for _, ok := range []string{"https://203.0.113.1:18443", "https://master.example:443", "https://203.0.113.1", "https://203.0.113.1:18443/"} {
		if err := ValidPublicURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://203.0.113.1:18443", "https://", "https://203.0.113.1:18443/x", "https://u:p@h:1",
		"https://h:1?x=1", "https://[2001:db8::1]:18443", "https://h:0", "https://h:99999", "https://h_$(id):1", "203.0.113.1:18443"} {
		if err := ValidPublicURL(bad); err == nil {
			t.Errorf("%q: ждали ошибку", bad)
		}
	}
}

func TestValidateResources(t *testing.T) {
	if err := ValidateResources(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResources([]string{"telemt.users", "mtg.config"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{"xui.clients"}, {"telemt.users", "telemt.users"}, {""}} {
		if err := ValidateResources(bad); err == nil {
			t.Errorf("%v: ждали ошибку", bad)
		}
	}
}

func TestValidCodeAndToken(t *testing.T) {
	for _, ok := range []string{"ps1", "pr-2", "a1"} {
		if !ValidCode(ok) {
			t.Errorf("%q: ждали ок", ok)
		}
	}
	for _, bad := range []string{"", "a", "1ps", "Ps1", "ps_1", "ps 1", strings.Repeat("a", 33)} {
		if ValidCode(bad) {
			t.Errorf("%q: ждали отказ", bad)
		}
	}
	if !ValidToken(tok) || ValidToken(strings.ToUpper(tok)) || ValidToken(tok[:63]) || ValidToken("../"+tok[3:]) {
		t.Fatal("проверка токена")
	}
}

func TestCommand(t *testing.T) {
	direct, err := DirectRoute("https://203.0.113.1:18443/", spki)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := Command(direct, tok)
	if err != nil {
		t.Fatal(err)
	}
	want := "curl -fsSLk --pinnedpubkey 'sha256//" + spki + "' https://203.0.113.1:18443/v1/join/" + tok + " | sudo bash"
	if cmd != want {
		t.Fatalf("команда выхода:\n%s\nждали\n%s", cmd, want)
	}
	bridge, err := BridgeRoute("203.0.113.7", "socks5://127.0.0.1:10872")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err = Command(bridge, tok)
	if err != nil {
		t.Fatal(err)
	}
	want = "curl -fsSL -x socks5://127.0.0.1:10872 http://203.0.113.7:18480/v1/join/" + tok + " | sudo bash"
	if cmd != want {
		t.Fatalf("команда релея:\n%s\nждали\n%s", cmd, want)
	}
	if _, err := Command(bridge, "bad"); err == nil {
		t.Fatal("плохой токен - ждали ошибку")
	}
	if _, err := Command(Route{Base: "https://h:1", Pin: "x'; id; '"}, tok); err == nil {
		t.Fatal("мусор в отпечатке - ждали ошибку")
	}
	if _, err := BridgeRoute("1.2.3.4;rm", "socks5://127.0.0.1:1"); err == nil {
		t.Fatal("мусор в адресе - ждали ошибку")
	}
	if _, err := Command(Route{Base: "http://1.2.3.4;rm:18480", Proxy: "socks5://127.0.0.1:1"}, tok); err == nil {
		t.Fatal("мусор в адресе маршрута - ждали ошибку")
	}
}

func TestEnv(t *testing.T) {
	env, err := Env(EnvParams{Target: "ps1", Kind: "exit", Resources: []string{"telemt.users"},
		PublicURL: "https://203.0.113.1:18443", MasterPin: hexp})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"TELEMUX_AGENT_TARGET=ps1\n", "TELEMUX_AGENT_MASTERS=https://203.0.113.1:18443\n",
		"TELEMUX_AGENT_MASTER_PIN=" + hexp + "\n", "TELEMUX_AGENT_TOKEN_FILE=/etc/telemux/agent.token\n",
		"TELEMUX_AGENT_USERS_FILE=/etc/telemt/users.toml\n", "TELEMUX_AGENT_MTG_FILE=none\n", "TELEMUX_AGENT_MODE=compare\n",
	} {
		if !strings.Contains(env, line) {
			t.Errorf("в agent.env выхода нет %q:\n%s", line, env)
		}
	}
	env, err = Env(EnvParams{Target: "pr1", Kind: "relay", Resources: []string{"mtg.config"}, Exits: []Exit{
		{Address: "203.0.113.1", Socks: "socks5://127.0.0.1:10871"}, {Address: "203.0.113.2", Socks: "socks5://127.0.0.1:10872"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"TELEMUX_AGENT_MASTERS=http://203.0.113.1:18480|socks5://127.0.0.1:10871,http://203.0.113.2:18480|socks5://127.0.0.1:10872\n",
		"TELEMUX_AGENT_USERS_FILE=none\n", "TELEMUX_AGENT_MTG_FILE=/etc/mtg-multi/config.toml\n", "TELEMUX_AGENT_MODE=compare\n",
	} {
		if !strings.Contains(env, line) {
			t.Errorf("в agent.env релея нет %q:\n%s", line, env)
		}
	}
	if strings.Contains(env, "MASTER_PIN") {
		t.Error("релею отпечаток мастера не нужен: он ходит к мосту по http")
	}
	// Все ключи - из тех, что знает агент (deploy/agent.env.example).
	example, err := os.ReadFile("../../deploy/agent.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(env, "\n") {
		if key, _, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") && !strings.Contains(string(example), "\n"+key+"=") {
			t.Errorf("ключа %s нет в deploy/agent.env.example", key)
		}
	}
	bad := []EnvParams{
		{Target: "pr1", Kind: "relay"},
		{Target: "pr1", Kind: "relay", Exits: []Exit{{Address: "1.2.3.4;rm", Socks: "socks5://127.0.0.1:1"}}},
		{Target: "ps1", Kind: "exit", PublicURL: "http://x:1", MasterPin: hexp},
		{Target: "ps1", Kind: "exit", PublicURL: "https://x:1", MasterPin: "nope"},
		{Target: "pan1", Kind: "panel"},
		{Target: "Bad", Kind: "exit", PublicURL: "https://x:1", MasterPin: hexp},
	}
	for _, p := range bad {
		if _, err := Env(p); err == nil {
			t.Errorf("%+v: ждали ошибку", p)
		}
	}
}

// Юнит в скрипте - тот же, что в deploy/.
func TestAgentUnitMatchesDeploy(t *testing.T) {
	want, err := os.ReadFile("../../deploy/telemux-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	if AgentUnit() != string(want) {
		t.Fatal("internal/agentjoin/telemux-agent.service разошёлся с deploy/telemux-agent.service")
	}
}

func TestScript(t *testing.T) {
	direct, _ := DirectRoute("https://203.0.113.1:18443", spki)
	bridge, _ := BridgeRoute("203.0.113.7", "socks5://127.0.0.1:10872")
	cases := []struct {
		name    string
		route   Route
		want    []string
		notWant []string
	}{
		{"выход", direct,
			[]string{"BASE='https://203.0.113.1:18443'", "'-k' '--pinnedpubkey' 'sha256//" + spki + "'"},
			[]string{"-x", "socks5"}},
		{"релей", bridge,
			[]string{"BASE='http://203.0.113.7:18480'", "'-x' 'socks5://127.0.0.1:10872'"},
			[]string{"pinnedpubkey", "https://"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := Script(c.route, tok, "ps1")
			if err != nil {
				t.Fatal(err)
			}
			s := string(raw)
			common := []string{
				"set -euo pipefail", "TOKEN='" + tok + "'", "TARGET='ps1'",
				`[ "$(id -u)" -eq 0 ]`, `[ "$(uname -m)" = x86_64 ]`,
				`"$BASE/v1/join/$TOKEN/telemux"`, `mv -f -- "$bin_tmp" /usr/local/bin/telemux`,
				`-X POST -o "$resp" "$BASE/v1/join/$TOKEN/agent?format=env"`,
				"agent.env.bak-$stamp", "chmod 600 /etc/telemux/agent.token.new /etc/telemux/agent.env.new",
				"systemctl daemon-reload", "systemctl enable --now telemux-agent", "journalctl -u telemux-agent",
				"ExecStart=/usr/local/bin/telemux agent", "режиме compare",
			}
			for _, w := range append(common, c.want...) {
				if !strings.Contains(s, w) {
					t.Errorf("в скрипте нет %q", w)
				}
			}
			for _, nw := range c.notWant {
				if strings.Contains(s, nw) {
					t.Errorf("в скрипте лишнее %q", nw)
				}
			}
			if strings.Contains(s, "jq") || strings.Contains(s, "python") {
				t.Error("скрипт не должен зависеть от jq или python")
			}
			bashSyntax(t, raw)
		})
	}
	// Непроверенные значения в скрипт не попадают.
	if _, err := Script(direct, tok, "ps1'; rm -rf / #"); err == nil {
		t.Fatal("мусор в коде машины - ждали ошибку")
	}
	if _, err := Script(direct, "$(id)", "ps1"); err == nil {
		t.Fatal("мусор в токене - ждали ошибку")
	}
	if _, err := Script(Route{Base: "https://203.0.113.1:1", Pin: spki, Proxy: "socks5://127.0.0.1:1"}, tok, "ps1"); err == nil {
		t.Fatal("маршрут сразу с отпечатком и прокси - ждали ошибку")
	}
	if _, err := Script(Route{Base: "https://x:1'$(id)'", Pin: spki}, tok, "ps1"); err == nil {
		t.Fatal("мусор в адресе мастера - ждали ошибку")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`a'b`); got != `'a'\''b'` {
		t.Fatalf("экранирование: %s", got)
	}
}

// bashSyntax - bash -n, если bash есть.
func bashSyntax(t *testing.T, script []byte) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Log("bash не найден - проверка синтаксиса пропущена")
		return
	}
	f := filepath.Join(t.TempDir(), "join.sh")
	if err := os.WriteFile(f, script, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// fakeRun - записывает вызовы iptables и отвечает заданными кодами.
type fakeRun struct {
	calls [][]string
	codes []int
	err   error
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) (int, []byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.err != nil {
		return -1, nil, f.err
	}
	code := f.codes[0]
	f.codes = f.codes[1:]
	return code, []byte("iptables: out"), nil
}

func TestIPTables(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.5")
	rule := []string{"TELEMUX-AGENTS", "-s", "203.0.113.5", "-p", "tcp", "--dport", "18443", "-j", "ACCEPT"}

	// Правила нет - -C (код 1), затем -I.
	f := &fakeRun{codes: []int{1, 0}}
	if err := (IPTables{Run: f.run}).Allow(context.Background(), "TELEMUX-AGENTS", ip, 18443); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append([]string{"iptables", "-w", "-C"}, rule...),
		append([]string{"iptables", "-w", "-I"}, rule...),
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("вызовы:\n%v\nждали\n%v", f.calls, want)
	}
	// Правило уже есть - только -C.
	f = &fakeRun{codes: []int{0}}
	if err := (IPTables{Run: f.run}).Allow(context.Background(), "TELEMUX-AGENTS", ip, 18443); err != nil || len(f.calls) != 1 {
		t.Fatalf("правило есть: %v, вызовов %d", err, len(f.calls))
	}
	// Ошибки: нет цепочки (код 2), -I не прошёл, iptables не запустился.
	for _, c := range []*fakeRun{{codes: []int{2}}, {codes: []int{1, 4}}, {err: errors.New("нет iptables")}} {
		if err := (IPTables{Run: c.run}).Allow(context.Background(), "TELEMUX-AGENTS", ip, 18443); err == nil {
			t.Errorf("%+v: ждали ошибку", c)
		}
	}
	// Мусор до iptables не доходит.
	f = &fakeRun{codes: []int{0}}
	for _, chain := range []string{"", "a b", "x;rm", strings.Repeat("a", 29)} {
		if err := (IPTables{Run: f.run}).Allow(context.Background(), chain, ip, 18443); err == nil {
			t.Errorf("цепочка %q: ждали ошибку", chain)
		}
	}
	if err := (IPTables{Run: f.run}).Allow(context.Background(), "C", netip.MustParseAddr("2001:db8::1"), 1); err == nil {
		t.Error("IPv6: ждали ошибку")
	}
	if err := (IPTables{Run: f.run}).Allow(context.Background(), "C", ip, 0); err == nil {
		t.Error("порт 0: ждали ошибку")
	}
	if len(f.calls) != 0 {
		t.Fatalf("iptables вызван с непроверенными аргументами: %v", f.calls)
	}
}

// Настоящий запуск: код выхода отличается от ошибки запуска.
func TestExecRunner(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("нет sh")
	}
	code, _, err := ExecRunner(context.Background(), "sh", "-c", "exit 1")
	if err != nil || code != 1 {
		t.Fatalf("exit 1: код %d, %v", code, err)
	}
	if _, _, err := ExecRunner(context.Background(), "/nonexistent/iptables"); err == nil {
		t.Fatal("нет бинаря - ждали ошибку")
	}
}
