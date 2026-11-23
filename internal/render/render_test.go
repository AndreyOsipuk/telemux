package render

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// goldenInput - testdata/cases.json: те же входы, из которых golden.ts получил
// эталоны кодом бота.
type goldenInput struct {
	Now   time.Time `json:"now"`
	Users []struct {
		Key                     string     `json:"key"`
		Secret                  string     `json:"secret"`
		ExpiresAt               *time.Time `json:"expires_at"`
		PreviousSecret          string     `json:"previous_secret"`
		PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at"`
	} `json:"users"`
	Cases []struct {
		Name   string `json:"name"`
		Engine string `json:"engine"`
		Server struct {
			No              int               `json:"no"`
			TLSDomain       string            `json:"tls_domain"`
			ExtraTLSDomains []string          `json:"extra_tls_domains"`
			NodeRetired     bool              `json:"node_retired"`
			Relay           bool              `json:"relay"`
			Settings        map[string]string `json:"settings"`
			Endpoints       []store.Endpoint  `json:"endpoints"`
		} `json:"server"`
		Machine Machine `json:"machine"`
	} `json:"cases"`
}

func loadGolden(t *testing.T) goldenInput {
	t.Helper()
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var in goldenInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

func sp(s string) *string { return &s }

// Сборщики совпадают с ботом байт в байт на всех эталонах.
func TestGolden(t *testing.T) {
	in := loadGolden(t)
	var users []User
	for _, u := range in.Users {
		users = append(users, User{Key: u.Key, Secret: u.Secret, ExpiresAt: u.ExpiresAt,
			PreviousSecret: u.PreviousSecret, PreviousSecretExpiresAt: u.PreviousSecretExpiresAt})
	}
	for _, c := range in.Cases {
		t.Run(c.Name, func(t *testing.T) {
			srv := store.Server{No: c.Server.No, Engine: c.Engine, State: "active", TLSDomain: c.Server.TLSDomain,
				ExtraTLSDomains: c.Server.ExtraTLSDomains, NodeRetired: c.Server.NodeRetired,
				Settings: c.Server.Settings, Endpoints: c.Server.Endpoints, NodeCode: sp("node")}
			if c.Server.Relay || c.Machine.Kind == "relay" {
				srv.RelayCode = sp("relay")
			}
			var got []byte
			var err error
			if c.Engine == "telemt" {
				got, err = TelemtMain(srv, c.Machine)
			} else {
				got, err = MtgConfig(srv, c.Machine, users, MtgOpts{Now: in.Now})
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", c.Name+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("не совпало с эталоном бота:\n%s", firstDiff(string(want), string(got)))
			}
		})
	}
}

// firstDiff - первая отличающаяся строка (для понятного падения).
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "строка " + strconv.Itoa(i+1) + ":\n  бот:     " + a + "\n  telemux: " + b
		}
	}
	return "отличие в переводах строк"
}

func telemtSrv() store.Server {
	return store.Server{No: 1, Engine: "telemt", State: "active", TLSDomain: "www.example.com",
		Settings: map[string]string{}, NodeCode: sp("ps1"), RelayCode: sp("pr1")}
}

// Нет адреса машины - ошибка no_address; не IP - тоже.
func TestTelemtMain_NoAddress(t *testing.T) {
	for _, addr := range []string{"", "  ", "relay-a"} {
		_, err := TelemtMain(telemtSrv(), Machine{Code: "pr1", Kind: "relay", Address: addr})
		if ErrorCode(err) != CodeNoAddress {
			t.Fatalf("адрес %q: ждали no_address, получили %v", addr, err)
		}
	}
}

// Настройки сервера, которых бот не менял (хранились у него в коде), подставляются.
func TestTelemtMain_Settings(t *testing.T) {
	srv := telemtSrv()
	srv.Settings = map[string]string{"max_connections": "777", "client_mss": "92", "rst_on_close": "always",
		"tg_connect": "15", "mask_timing_floor_ms": "10"}
	got, err := TelemtMain(srv, Machine{Code: "ps1", Kind: "exit", Address: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\nmax_connections = 777\n", "\nclient_mss = \"92\"\n", "\nrst_on_close = \"always\"\n",
		"\ntg_connect = 15\n", "\nmask_timing_normalization_floor_ms = 10\n", "\nlisten_backlog = 8192\n"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("нет %q", want)
		}
	}
	if strings.Contains(string(got), "{{") {
		t.Fatal("осталась неподставленная метка")
	}
}

// Кавычка в строковой настройке сломала бы TOML - ошибка, а не конфиг.
func TestTelemtMain_Quote(t *testing.T) {
	srv := telemtSrv()
	srv.Settings = map[string]string{"client_mss": `x" = 1`}
	_, err := TelemtMain(srv, Machine{Code: "ps1", Kind: "exit", Address: "192.0.2.1"})
	if ErrorCode(err) != CodeBadSetting {
		t.Fatalf("ждали bad_setting, получили %v", err)
	}
	srv = telemtSrv()
	srv.TLSDomain = ""
	if _, err := TelemtMain(srv, Machine{Code: "ps1", Kind: "exit", Address: "192.0.2.1"}); ErrorCode(err) != CodeNoTLSDomain {
		t.Fatalf("ждали no_tls_domain, получили %v", err)
	}
	if _, err := MtgConfig(srv, Machine{Code: "ps1", Kind: "exit"}, nil, MtgOpts{}); ErrorCode(err) != CodeWrongEngine {
		t.Fatalf("ждали wrong_engine, получили %v", err)
	}
}

// Пользователь без срока остаётся; пустой список - пустой [secrets]; мусорный ключ
// пропускается (сломал бы TOML).
func TestMtgConfig_Users(t *testing.T) {
	srv := store.Server{No: 5, Engine: "mtg", State: "active", TLSDomain: "static-a.example.com", Settings: map[string]string{}}
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	users := []User{
		{Key: "forever_1", Secret: "0123456789abcdef0123456789abcdef"},
		{Key: "bad\"key", Secret: "0123456789abcdef0123456789abcdef"},
		{Key: "expired_2", Secret: "0123456789abcdef0123456789abcdef", ExpiresAt: &past},
		{Key: "exact_3", Secret: "0123456789abcdef0123456789abcdef", ExpiresAt: &now},
	}
	got, err := MtgConfig(srv, Machine{Code: "ps5", Kind: "exit"}, users, MtgOpts{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	_, secrets, _ := strings.Cut(s, "[secrets]\n")
	if secrets != "\"forever_1\" = \"ee0123456789abcdef0123456789abcdef7374617469632d612e6578616d706c652e636f6d\"\n" {
		t.Fatalf("[secrets]:\n%s", secrets)
	}
	empty, err := MtgConfig(srv, Machine{Code: "ps5", Kind: "exit"}, nil, MtgOpts{Now: now})
	if err != nil || !strings.HasSuffix(string(empty), "[secrets]\n") {
		t.Fatalf("пустой список: %v\n%s", err, empty)
	}
	// Релей без local-mtg своего mtg не имеет.
	if _, err := MtgConfig(srv, Machine{Code: "pr5", Kind: "relay"}, nil, MtgOpts{Now: now}); ErrorCode(err) != CodeBadSetting {
		t.Fatalf("ждали bad_setting, получили %v", err)
	}
}

// Какие машины и ресурсы собираются у сервера.
func TestJobs(t *testing.T) {
	tm := telemtSrv()
	if got := Jobs(tm); len(got) != 2 || got[0] != (Job{Target: "ps1", Kind: "exit", Resource: ResourceTelemtConfig}) || got[1] != (Job{Target: "pr1", Kind: "relay", Resource: ResourceTelemtConfig}) {
		t.Fatalf("telemt: %+v", got)
	}
	tm.NodeRetired = true
	if got := Jobs(tm); len(got) != 1 || got[0].Target != "pr1" {
		t.Fatalf("telemt node_retired: %+v", got)
	}
	tm.State = "disabled"
	if got := Jobs(tm); len(got) != 0 {
		t.Fatalf("disabled: %+v", got)
	}
	mg := store.Server{No: 2, Engine: "mtg", State: "test", NodeCode: sp("ps2"), RelayCode: sp("pr2"), Settings: map[string]string{}}
	if got := Jobs(mg); len(got) != 1 || got[0] != (Job{Target: "ps2", Kind: "exit", Resource: ResourceMtgConfig}) {
		t.Fatalf("mtg без local-mtg: %+v", got)
	}
	mg.Settings = map[string]string{"RELAY_MODE": "local-mtg", "BACKEND": "xui"}
	if got := Jobs(mg); len(got) != 1 || got[0] != (Job{Target: "pr2", Kind: "relay", Resource: ResourceMtgConfig}) {
		t.Fatalf("mtg xui + local-mtg: %+v", got)
	}
	var e *Error
	if !errors.As(errf(CodeNoAddress, "x"), &e) || ErrorCode(errors.New("x")) != "internal" {
		t.Fatal("ErrorCode")
	}
}
