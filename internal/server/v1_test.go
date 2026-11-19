package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

// fakeRevisions - in-memory ревизии поверх fakeUsers.
type fakeRevisions struct {
	mu          sync.Mutex
	users       *fakeUsers
	rev         int64
	published   int
	status      map[int64][]store.TargetStatus
	lastContent string
}

func (f *fakeRevisions) UpsertUser(_ context.Context, r store.ImportRow) (bool, error) {
	f.users.mu.Lock()
	defer f.users.mu.Unlock()
	_, existed := f.users.m[r.Username]
	f.users.m[r.Username] = store.User{Username: r.Username, Secret: r.Secret, ExpirationAt: r.ExpirationAt, MaxTCPConns: r.MaxTCPConns, Enabled: true}
	return !existed, nil
}

func (f *fakeRevisions) PublishTelemtUsers(_ context.Context, _ int) (store.PublishResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev++
	f.published++
	return store.PublishResult{Revision: f.rev, Changed: []string{"ps2", "pr2"}}, nil
}

func (f *fakeRevisions) RevisionStatus(_ context.Context, rev int64) ([]store.TargetStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.status[rev]
	if !ok {
		return nil, store.ErrUnknownRevision
	}
	return st, nil
}

const botToken = "bot-secret"

func v1Server(inRecovery bool, users int) (*Server, *fakeRevisions) {
	fu := newFakeUsers(users)
	fr := &fakeRevisions{users: fu, status: map[int64][]store.TargetStatus{}}
	s := New(Deps{
		Store: fakeStore{inRecovery: inRecovery}, Users: fu, Revisions: fr, Node: &fakeNode{},
		Version: "v1", SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow},
		BotToken: botToken, DefaultMaxConns: 50,
		AdminUser: "admin", AdminPassword: "pw", // авторизация панели включена
	})
	return s, fr
}

const (
	s1 = "11111111111111111111111111111111"
	s2 = "22222222222222222222222222222222"
)

func usersBody(keys ...string) string {
	var parts []string
	for i, k := range keys {
		sec := s1
		if i%2 == 1 {
			sec = s2
		}
		parts = append(parts, `{"key":"`+k+`","secret":"`+sec+`","expires_at":"2027-01-01T00:00:00Z"}`)
	}
	return `{"users":[` + strings.Join(parts, ",") + `]}`
}

// Полный набор от бота: сверка пользователей + публикация ревизии.
func TestV1_PutUsers_ReconcilesAndPublishes(t *testing.T) {
	s, fr := v1Server(false, 0)
	rec := do(s, "PUT", "/api/v1/users", botToken, usersBody("alice_1", "bob_2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Revision int64    `json:"revision"`
		Inserted int      `json:"inserted"`
		Changed  []string `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Revision != 1 || out.Inserted != 2 || len(out.Changed) != 2 || fr.published != 1 {
		t.Fatalf("ждали ревизию 1, 2 вставки, 2 машины; получили %+v, публикаций %d", out, fr.published)
	}
}

// Без токена бота и без сессии - 401; чужой токен - 401.
func TestV1_PutUsers_RequiresAuth(t *testing.T) {
	s, _ := v1Server(false, 0)
	for _, tok := range []string{"", "wrong"} {
		if rec := do(s, "PUT", "/api/v1/users", tok, usersBody("alice_1")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("токен %q → ждали 401, получили %d", tok, rec.Code)
		}
	}
}

// Мусор в ключе или секрете - 400 с указанием пользователя, ничего не пишется.
func TestV1_PutUsers_ValidatesInput(t *testing.T) {
	s, fr := v1Server(false, 0)
	for _, body := range []string{
		`{"users":[{"key":"bad key","secret":"` + s1 + `"}]}`,
		`{"users":[{"key":"alice_1","secret":"short"}]}`,
		`{"users":[{"key":"alice_1","secret":"` + s1 + `"},{"key":"alice_1","secret":"` + s2 + `"}]}`,
		`{"users":`,
	} {
		rec := do(s, "PUT", "/api/v1/users", botToken, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d", body, rec.Code)
		}
		if e := decodeErr(t, rec); e.Code != "bad_request" {
			t.Fatalf("ждали code=bad_request, получили %q", e.Code)
		}
	}
	if fr.published != 0 {
		t.Fatal("при ошибке ввода публикаций быть не должно")
	}
}

// Слишком большой набор - 400 до разбора пользователей.
func TestV1_PutUsers_BatchLimit(t *testing.T) {
	s, _ := v1Server(false, 0)
	var sb strings.Builder
	sb.WriteString(`{"users":[`)
	for i := 0; i <= store.MaxImportBatch; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"key":"u_` + itoa(i) + `","secret":"` + s1 + `"}`)
	}
	sb.WriteString(`]}`)
	rec := do(s, "PUT", "/api/v1/users", botToken, sb.String())
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("ждали 400/413 на слишком большой набор, получили %d", rec.Code)
	}
}

// Защита от массового удаления - 409 с кодом mass_delete_guard, публикации нет.
func TestV1_PutUsers_MassDeleteGuard(t *testing.T) {
	s, fr := v1Server(false, 20)
	rec := do(s, "PUT", "/api/v1/users", botToken, usersBody("alice_1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("ждали 409, получили %d", rec.Code)
	}
	if e := decodeErr(t, rec); e.Code != "mass_delete_guard" {
		t.Fatalf("ждали code=mass_delete_guard, получили %q", e.Code)
	}
	if fr.published != 0 {
		t.Fatal("при срабатывании защиты публикации быть не должно")
	}
	// С force=true проходит.
	if rec := do(s, "PUT", "/api/v1/users?force=true", botToken, usersBody("alice_1")); rec.Code != http.StatusOK {
		t.Fatalf("force=true → ждали 200, получили %d", rec.Code)
	}
}

// Запись на не-мастере - 409 not_master.
func TestV1_WritesBlockedOnReplica(t *testing.T) {
	s, _ := v1Server(true, 0)
	for _, c := range []struct{ m, p, b string }{
		{"PUT", "/api/v1/users", usersBody("alice_1")},
		{"PUT", "/api/v1/users/alice_1", `{"secret":"` + s1 + `"}`},
		{"DELETE", "/api/v1/users/alice_1", ""},
	} {
		rec := do(s, c.m, c.p, botToken, c.b)
		if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != "not_master" {
			t.Fatalf("%s %s на реплике → ждали 409 not_master, получили %d %s", c.m, c.p, rec.Code, rec.Body.String())
		}
	}
}

// Точечные изменения: создать, изменить, удалить - каждое публикует ревизию.
func TestV1_PointUserChanges(t *testing.T) {
	s, fr := v1Server(false, 0)
	rec := do(s, "PUT", "/api/v1/users/alice_1", botToken, `{"secret":"`+s1+`","expires_at":"2027-01-01T00:00:00Z"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("новый пользователь → ждали 201, получили %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, "PUT", "/api/v1/users/alice_1", botToken, `{"secret":"`+s2+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("изменение → ждали 200, получили %d", rec.Code)
	}
	if fr.users.m["alice_1"].Secret != s2 {
		t.Fatal("секрет не изменился")
	}
	if rec := do(s, "PUT", "/api/v1/users/bad%20key", botToken, `{"secret":"`+s1+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("плохой ключ → ждали 400, получили %d", rec.Code)
	}
	if rec := do(s, "DELETE", "/api/v1/users/alice_1", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("удаление → ждали 200, получили %d", rec.Code)
	}
	if rec := do(s, "DELETE", "/api/v1/users/alice_1", botToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("повторное удаление → ждали 404, получили %d", rec.Code)
	}
	if fr.published != 3 {
		t.Fatalf("ждали 3 публикации (создать, изменить, удалить), получили %d", fr.published)
	}
}

// Статус ревизии: сколько машин применило.
func TestV1_RevisionStatus(t *testing.T) {
	s, fr := v1Server(false, 0)
	r1 := int64(1)
	fr.status[7] = []store.TargetStatus{
		{Code: "ps2", DesiredRevision: 7, ObservedRevision: &r1, Status: "applied", Applied: false},
		{Code: "pr2", DesiredRevision: 7, Status: "applied", Applied: true},
	}
	rec := do(s, "GET", "/api/v1/revisions/7/status", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Revision int64                `json:"revision"`
		Applied  int                  `json:"applied"`
		Total    int                  `json:"total"`
		Targets  []store.TargetStatus `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Revision != 7 || out.Applied != 1 || out.Total != 2 || len(out.Targets) != 2 {
		t.Fatalf("ждали 1 из 2, получили %+v", out)
	}
	if rec := do(s, "GET", "/api/v1/revisions/999/status", botToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("неизвестная ревизия → ждали 404, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/v1/revisions/abc/status", botToken, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("мусор в номере → ждали 400, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/v1/revisions/7/status", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена → ждали 401, получили %d", rec.Code)
	}
}

func (f *fakeRevisions) PublishResource(_ context.Context, target, resource string, content []byte) (store.PublishResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case target == "nobody":
		return store.PublishResult{}, store.ErrUnknownTarget
	case target == "ps2":
		return store.PublishResult{}, store.ErrResourceNotAssigned
	}
	f.rev++
	f.published++
	f.lastContent = string(content)
	return store.PublishResult{Revision: f.rev, Changed: []string{target}}, nil
}

// Бот публикует готовый конфиг mtg машины.
func TestV1_PutTargetResource(t *testing.T) {
	s, fr := v1Server(false, 0)
	cfg := "[secrets]\n\"alice_1\" = \"ee11\"\n"
	rec := do(s, "PUT", "/api/v1/targets/pr2/resources/mtg.config", botToken, cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	if fr.lastContent != cfg || fr.published != 1 {
		t.Fatalf("содержимое не дошло: %q", fr.lastContent)
	}
	cases := []struct {
		path   string
		status int
	}{
		{"/api/v1/targets/pr2/resources/telemt.users", http.StatusBadRequest}, // telemt.users собирает сам telemux
		{"/api/v1/targets/pr2/resources/unknown.thing", http.StatusBadRequest},
		{"/api/v1/targets/ps2/resources/mtg.config", http.StatusBadRequest},  // ресурс не назначен машине
		{"/api/v1/targets/nobody/resources/mtg.config", http.StatusNotFound}, // нет машины
	}
	for _, c := range cases {
		if rec := do(s, "PUT", c.path, botToken, cfg); rec.Code != c.status {
			t.Fatalf("%s → ждали %d, получили %d", c.path, c.status, rec.Code)
		}
	}
	if rec := do(s, "PUT", "/api/v1/targets/pr2/resources/mtg.config", botToken, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("пустой конфиг → ждали 400, получили %d", rec.Code)
	}
	if rec := do(s, "PUT", "/api/v1/targets/pr2/resources/mtg.config", "", cfg); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена → ждали 401, получили %d", rec.Code)
	}
	replica, _ := v1Server(true, 0)
	if rec := do(replica, "PUT", "/api/v1/targets/pr2/resources/mtg.config", botToken, cfg); rec.Code != http.StatusConflict {
		t.Fatalf("на реплике → ждали 409, получили %d", rec.Code)
	}
}

// Основной конфиг telemt от бота: проверяется до публикации - без WEB-профилей
// (их собирает агент из telemt.users), с include на users.toml, разбирается.
func TestV1_PutTargetResource_TelemtConfig(t *testing.T) {
	s, fr := v1Server(false, 0)
	cfg := "[server]\nport = 443\n\n[access]\ninclude = \"/etc/telemt/users.toml\"\n"
	rec := do(s, "PUT", "/api/v1/targets/pr2/resources/telemt.config", botToken, cfg)
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	if fr.lastContent != cfg {
		t.Fatalf("содержимое не дошло: %q", fr.lastContent)
	}
	for name, bad := range map[string]string{
		"профили":     cfg + "\n[[web.vhosts]]\nhost = \"x\"\n\n[[web.vhosts.profiles]]\nuser = \"a\"\n",
		"без include": "[server]\nport = 443\n",
		"битый TOML":  cfg + "[server\n",
	} {
		before := fr.published
		if rec := do(s, "PUT", "/api/v1/targets/pr2/resources/telemt.config", botToken, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: ждали 400, получили %d", name, rec.Code)
		}
		if fr.published != before {
			t.Errorf("%s: отвергнутый конфиг опубликован", name)
		}
	}
}
