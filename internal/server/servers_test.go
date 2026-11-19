package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AndreyOsipuk/telemux/internal/store"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
)

// fakeServers - справочник серверов в памяти с теми же ошибками, что у store.
type fakeServers struct {
	servers map[int]store.Server
	relays  map[string]bool
	exits   map[string]bool
	err     error // все операции падают с этой ошибкой
	writes  int   // сколько раз дошло до записи
}

func newFakeServers() *fakeServers {
	return &fakeServers{servers: map[int]store.Server{}, relays: map[string]bool{"pr1": true}, exits: map[string]bool{"ps1": true}}
}

func (f *fakeServers) machines(srv store.Server) error {
	if c := srv.RelayCode; c != nil && !f.relays[*c] {
		return fmt.Errorf("%w: relay_code %q", store.ErrUnknownMachine, *c)
	}
	if c := srv.NodeCode; c != nil && !f.exits[*c] {
		return fmt.Errorf("%w: node_code %q", store.ErrUnknownMachine, *c)
	}
	return nil
}

func (f *fakeServers) ListServers(_ context.Context, engine, state string, limit, offset int) ([]store.Server, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	var all []store.Server
	for _, s := range f.servers {
		if (engine == "" || s.Engine == engine) && (state == "" || s.State == state) {
			all = append(all, s)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].No < all[j].No })
	total := len(all)
	if offset > total {
		offset = total
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	return all, total, nil
}

func (f *fakeServers) GetServer(_ context.Context, no int) (store.Server, error) {
	if f.err != nil {
		return store.Server{}, f.err
	}
	s, ok := f.servers[no]
	if !ok {
		return s, fmt.Errorf("%w: %d", store.ErrServerNotFound, no)
	}
	return s, nil
}

func (f *fakeServers) put(w store.ServerWrite) (store.Server, error) {
	srv, err := w.Server()
	if err != nil {
		return srv, err
	}
	if err := f.machines(srv); err != nil {
		return srv, err
	}
	now := time.Now().UTC()
	srv.CreatedAt, srv.UpdatedAt = now, now
	srv.ConfigSource = store.ConfigSourceBot
	relay := store.LiveMachine{Code: "pr1", SeenAt: &now, APIOK: true, Connections: 3, UsersOnline: 2}
	srv.Live = &store.ServerLive{}
	if srv.RelayCode != nil {
		srv.Live.Relay = &relay
	}
	return srv, nil
}

func (f *fakeServers) CreateServer(_ context.Context, w store.ServerWrite) (store.Server, error) {
	f.writes++
	if f.err != nil {
		return store.Server{}, f.err
	}
	if _, ok := f.servers[w.No]; ok {
		return store.Server{}, fmt.Errorf("%w: %d", store.ErrServerExists, w.No)
	}
	srv, err := f.put(w)
	if err != nil {
		return srv, err
	}
	f.servers[srv.No] = srv
	return srv, nil
}

func (f *fakeServers) ReplaceServer(_ context.Context, w store.ServerWrite) (store.Server, error) {
	f.writes++
	if f.err != nil {
		return store.Server{}, f.err
	}
	cur, ok := f.servers[w.No]
	if !ok {
		return store.Server{}, fmt.Errorf("%w: %d", store.ErrServerNotFound, w.No)
	}
	if cur.Engine != w.Engine {
		return store.Server{}, store.ErrEngineImmutable
	}
	srv, err := f.put(w)
	if err != nil {
		return srv, err
	}
	f.servers[srv.No] = srv
	return srv, nil
}

func (f *fakeServers) DeleteServer(_ context.Context, no int) error {
	f.writes++
	if f.err != nil {
		return f.err
	}
	cur, ok := f.servers[no]
	if !ok {
		return fmt.Errorf("%w: %d", store.ErrServerNotFound, no)
	}
	if cur.State != "disabled" {
		return store.ErrServerNotDisabled
	}
	delete(f.servers, no)
	return nil
}

func (f *fakeServers) ImportServers(_ context.Context, items []store.ServerWrite) (int, int, error) {
	f.writes++
	if f.err != nil {
		return 0, 0, f.err
	}
	next := map[int]store.Server{}
	for k, v := range f.servers {
		next[k] = v
	}
	ins, upd := 0, 0
	for _, w := range items {
		if cur, ok := next[w.No]; ok {
			if cur.Engine != w.Engine {
				return 0, 0, store.ErrEngineImmutable
			}
			upd++
		} else {
			ins++
		}
		srv, err := f.put(w)
		if err != nil {
			return 0, 0, err
		}
		next[srv.No] = srv
	}
	f.servers = next
	return ins, upd, nil
}

func serversServer(inRecovery bool) (*Server, *fakeServers) {
	fs := newFakeServers()
	s := New(Deps{
		Store: fakeStore{inRecovery: inRecovery}, Node: &fakeNode{}, Version: "v1",
		SyncOpts: syncpkg.Options{Mode: syncpkg.Shadow}, BotToken: botToken,
		AdminUser: "admin", AdminPassword: "pw", Servers: fs,
	})
	return s, fs
}

// serverJSON - корректное тело сервера с заменой полей (nil - удалить поле).
func serverJSON(no int, patch map[string]any) string {
	body := map[string]any{
		"no": no, "name": "Финляндия", "emoji": "🇫🇮", "country": "fi", "engine": "telemt",
		"relay_code": "pr1", "node_code": "ps1", "state": "active", "relay_only": true,
		"tls_domain": "www.example.com", "extra_tls_domains": []string{"a.example.com"},
		"settings": map[string]string{"port": "443", "ipv6": "1"},
		"note":     "перенесено вручную",
		"endpoints": []map[string]any{
			{"protocol": "ee", "host": "pr1.example.net", "port": 2091, "tls_domain": "www.example.com"},
			{"protocol": "dd", "host": "pr1.example.net", "port": 2091},
			{"protocol": "web", "host": "203.0.113.10"},
		},
	}
	for k, v := range patch {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func TestServers_CRUD(t *testing.T) {
	s, fs := serversServer(false)
	rec := do(s, "POST", "/api/v1/servers", botToken, serverJSON(1, nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("создание: %d %s", rec.Code, rec.Body.String())
	}
	var srv store.Server
	if err := json.Unmarshal(rec.Body.Bytes(), &srv); err != nil {
		t.Fatal(err)
	}
	if srv.No != 1 || len(srv.Endpoints) != 3 || srv.Endpoints[2].Port != 443 || !srv.Endpoints[2].ViaRelay {
		t.Fatalf("ответ создания: %+v", srv)
	}
	if srv.Live == nil || srv.Live.Relay == nil || srv.Live.Relay.Stale {
		t.Fatalf("живые цифры: свежий снимок не stale: %+v", srv.Live)
	}

	if rec := do(s, "GET", "/api/v1/servers/1", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("чтение: %d", rec.Code)
	}
	if rec := do(s, "PUT", "/api/v1/servers/1", botToken, serverJSON(0, map[string]any{"name": "Нидерланды", "state": "disabled"})); rec.Code != http.StatusOK {
		t.Fatalf("замена без no в теле: %d %s", rec.Code, rec.Body.String())
	}
	if fs.servers[1].Name != "Нидерланды" {
		t.Fatal("замена не дошла")
	}
	if rec := do(s, "DELETE", "/api/v1/servers/1", botToken, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("удаление выключенного: %d %s", rec.Code, rec.Body.String())
	}
	if len(fs.servers) != 0 {
		t.Fatal("сервер не удалён")
	}
}

func TestServers_ListAndFields(t *testing.T) {
	s, fs := serversServer(false)
	for no := 1; no <= 3; no++ {
		if rec := do(s, "POST", "/api/v1/servers", botToken, serverJSON(no, nil)); rec.Code != http.StatusCreated {
			t.Fatal(rec.Body.String())
		}
	}
	old := time.Now().Add(-time.Hour)
	srv := fs.servers[2]
	srv.Live.Relay.SeenAt = &old
	fs.servers[2] = srv

	rec := do(s, "GET", "/api/v1/servers?engine=telemt&state=active&limit=2&offset=1", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("список: %d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Data   []store.Server `json:"data"`
		Paging map[string]int `json:"paging"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Paging["total"] != 3 || len(page.Data) != 2 || page.Data[0].No != 2 {
		t.Fatalf("страница: %s", rec.Body.String())
	}
	if !page.Data[0].Live.Relay.Stale || page.Data[1].Live.Relay.Stale {
		t.Fatalf("stale по порогу: %s", rec.Body.String())
	}
	empty := do(s, "GET", "/api/v1/servers?state=disabled", botToken, "")
	if !strings.Contains(empty.Body.String(), `"data":[]`) {
		t.Fatalf("пустая страница - data: []: %s", empty.Body.String())
	}

	rec = do(s, "GET", "/api/v1/servers/fields?engine=mtg", botToken, "")
	var fields struct {
		Data []map[string]any `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &fields) != nil || len(fields.Data) == 0 {
		t.Fatalf("каталог: %d %s", rec.Code, rec.Body.String())
	}
	for _, f := range fields.Data {
		if f["engine"] != "mtg" {
			t.Fatalf("фильтр каталога по движку: %v", f)
		}
	}
}

// Каждый плохой ввод - свой 4xx с машинным кодом, до записи в хранилище.
func TestServers_BadInput(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{"не JSON", "POST", "/api/v1/servers", `{`, 400, "bad_request"},
		{"пустое имя", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"name": "  "}), 400, "bad_request"},
		{"номер 0", "POST", "/api/v1/servers", serverJSON(0, nil), 400, "bad_request"},
		{"номер 100", "POST", "/api/v1/servers", serverJSON(100, nil), 400, "bad_request"},
		{"движок", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"engine": "xray"}), 400, "bad_request"},
		{"состояние", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"state": "on"}), 400, "bad_request"},
		{"страна", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"country": "fin"}), 400, "bad_request"},
		{"лимит 0", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"user_max_tcp_conns": 0}), 400, "bad_request"},
		{"лимит IP -1", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"user_max_unique_ips": -1}), 400, "bad_request"},
		{"tls_domain", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"tls_domain": "*.example.com"}), 400, "bad_request"},
		{"extra домен", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"extra_tls_domains": []string{"a b"}}), 400, "bad_request"},
		{"длинная заметка", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"note": strings.Repeat("я", 1001)}), 400, "bad_request"},
		{"неизвестная настройка", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"settings": map[string]string{"nope": "1"}}), 400, "unknown_setting"},
		{"плохая настройка", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"settings": map[string]string{"port": "70000"}}), 400, "bad_setting"},
		{"протокол", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"endpoints": []map[string]any{{"protocol": "tls", "host": "a.example.com"}}}), 400, "bad_endpoint"},
		{"хост", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"endpoints": []map[string]any{{"protocol": "dd", "host": "not a host"}}}), 400, "bad_endpoint"},
		{"порт", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"endpoints": []map[string]any{{"protocol": "dd", "host": "a.example.com", "port": 0}}}), 400, "bad_endpoint"},
		{"ee без домена", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"endpoints": []map[string]any{{"protocol": "ee", "host": "a.example.com"}}}), 400, "bad_endpoint"},
		{"дубль точки входа", "POST", "/api/v1/servers", serverJSON(1, map[string]any{"endpoints": []map[string]any{
			{"protocol": "dd", "host": "a.example.com"}, {"protocol": "dd", "host": "A.example.com", "port": 443}}}), 400, "bad_endpoint"},
		{"нет релея", "POST", "/api/v1/servers", serverJSON(2, map[string]any{"relay_code": "pr9"}), 400, "unknown_machine"},
		{"выход вместо релея", "POST", "/api/v1/servers", serverJSON(2, map[string]any{"relay_code": "ps1"}), 400, "unknown_machine"},
		{"номер в пути - не число", "GET", "/api/v1/servers/abc", "", 404, "not_found"},
		{"нет сервера", "GET", "/api/v1/servers/42", "", 404, "not_found"},
		{"замена несуществующего", "PUT", "/api/v1/servers/42", serverJSON(42, nil), 404, "not_found"},
		{"номер в теле не тот", "PUT", "/api/v1/servers/1", serverJSON(2, nil), 400, "bad_request"},
		{"смена движка", "PUT", "/api/v1/servers/1", serverJSON(1, map[string]any{"engine": "mtg", "settings": map[string]string{}}), 400, "engine_immutable"},
		{"номер занят", "POST", "/api/v1/servers", serverJSON(1, nil), 409, "conflict"},
		{"удаление активного", "DELETE", "/api/v1/servers/1", "", 409, "conflict"},
		{"удаление несуществующего", "DELETE", "/api/v1/servers/42", "", 404, "not_found"},
		{"фильтр engine", "GET", "/api/v1/servers?engine=xray", "", 400, "bad_request"},
		{"фильтр state", "GET", "/api/v1/servers?state=on", "", 400, "bad_request"},
		{"limit 101", "GET", "/api/v1/servers?limit=101", "", 400, "bad_request"},
		{"каталог: движок", "GET", "/api/v1/servers/fields?engine=xray", "", 400, "bad_request"},
		{"импорт не массив", "PUT", "/api/v1/servers", `{"servers":[]}`, 400, "bad_request"},
		{"импорт плохая запись", "PUT", "/api/v1/servers", "[" + serverJSON(2, nil) + "," + serverJSON(3, map[string]any{"settings": map[string]string{"x": "1"}}) + "]", 400, "unknown_setting"},
		{"импорт повтор номера", "PUT", "/api/v1/servers", "[" + serverJSON(2, nil) + "," + serverJSON(2, nil) + "]", 400, "bad_request"},
		{"импорт неизвестная машина", "PUT", "/api/v1/servers", "[" + serverJSON(2, map[string]any{"node_code": "ps9"}) + "]", 400, "unknown_machine"},
		{"импорт смена движка", "PUT", "/api/v1/servers", "[" + serverJSON(1, map[string]any{"engine": "mtg", "settings": map[string]string{}}) + "]", 400, "engine_immutable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, fs := serversServer(false)
			if rec := do(s, "POST", "/api/v1/servers", botToken, serverJSON(1, nil)); rec.Code != http.StatusCreated {
				t.Fatal(rec.Body.String())
			}
			before := fs.servers[1]
			rec := do(s, c.method, c.path, botToken, c.body)
			if rec.Code != c.status {
				t.Fatalf("ждали %d, получили %d: %s", c.status, rec.Code, rec.Body.String())
			}
			if e := decodeErr(t, rec); e.Code != c.code {
				t.Fatalf("код ошибки: ждали %q, получили %q (%s)", c.code, e.Code, e.Message)
			}
			if len(fs.servers) != 1 || fs.servers[1].Name != before.Name || fs.servers[1].Engine != before.Engine {
				t.Fatalf("после ошибки изменилось хранилище: %+v", fs.servers)
			}
		})
	}
}

func TestServers_ImportLimits(t *testing.T) {
	s, fs := serversServer(false)
	items := make([]string, store.MaxServersImport+1)
	for i := range items {
		items[i] = serverJSON(i%99+1, nil)
	}
	rec := do(s, "PUT", "/api/v1/servers", botToken, "["+strings.Join(items, ",")+"]")
	if rec.Code != http.StatusBadRequest || decodeErr(t, rec).Code != "batch_too_large" {
		t.Fatalf("101 сервер: ждали 400 batch_too_large, получили %d %s", rec.Code, rec.Body.String())
	}
	if fs.writes != 0 {
		t.Fatal("батч сверх лимита дошёл до хранилища")
	}
	big := `[` + serverJSON(1, map[string]any{"note": strings.Repeat("x", maxServerImportBody)}) + `]`
	if rec := do(s, "PUT", "/api/v1/servers", botToken, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("большое тело: ждали 413, получили %d", rec.Code)
	}
	rec = do(s, "PUT", "/api/v1/servers", botToken, "["+serverJSON(1, nil)+","+serverJSON(2, nil)+"]")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"inserted":2`) {
		t.Fatalf("импорт: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(s, "PUT", "/api/v1/servers", botToken, "["+serverJSON(1, nil)+","+serverJSON(3, nil)+"]")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"inserted":1`) || !strings.Contains(rec.Body.String(), `"updated":1`) {
		t.Fatalf("повторный импорт: %d %s", rec.Code, rec.Body.String())
	}
}

// Доступ: без токена - 401, на реплике запись - 409 not_master без обращения к хранилищу.
func TestServers_AuthAndMaster(t *testing.T) {
	s, _ := serversServer(false)
	if rec := do(s, "GET", "/api/v1/servers", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/v1/servers/fields", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("каталог без токена: %d", rec.Code)
	}
	replica, fs := serversServer(true)
	for _, c := range [][3]string{
		{"POST", "/api/v1/servers", serverJSON(1, nil)},
		{"PUT", "/api/v1/servers/1", serverJSON(1, nil)},
		{"DELETE", "/api/v1/servers/1", ""},
		{"PUT", "/api/v1/servers", "[" + serverJSON(1, nil) + "]"},
	} {
		rec := do(replica, c[0], c[1], botToken, c[2])
		if rec.Code != http.StatusConflict || decodeErr(t, rec).Code != "not_master" {
			t.Fatalf("%s %s на реплике: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	if fs.writes != 0 {
		t.Fatal("запись на реплике дошла до хранилища")
	}
	if rec := do(replica, "GET", "/api/v1/servers", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("чтение на реплике: %d", rec.Code)
	}
}

// Сбой базы: недоступность - 503, ошибка ввода, отвергнутая базой, - 4xx, баг - 500.
func TestServers_StoreErrors(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
	}{
		{context.DeadlineExceeded, http.StatusServiceUnavailable},
		{&pgconn.PgError{Code: "23503"}, http.StatusBadRequest},
		{&pgconn.PgError{Code: "23505"}, http.StatusConflict},
		{errors.New("баг"), http.StatusInternalServerError},
	} {
		s, fs := serversServer(false)
		fs.err = fmt.Errorf("servers: %w", c.err)
		if rec := do(s, "GET", "/api/v1/servers", botToken, ""); rec.Code != c.status {
			t.Fatalf("%v: список - ждали %d, получили %d", c.err, c.status, rec.Code)
		}
		if rec := do(s, "POST", "/api/v1/servers", botToken, serverJSON(1, nil)); rec.Code != c.status {
			t.Fatalf("%v: создание - ждали %d, получили %d", c.err, c.status, rec.Code)
		}
	}
}

// Ответы и ошибки ручек серверов совпадают со спекой.
func TestContract_Servers(t *testing.T) {
	_, router := loadSpec(t)
	s, _ := serversServer(false)
	checkContract(t, router, s, "POST", "/api/v1/servers", serverJSON(1, nil), http.StatusCreated)
	// Сервер без машин: live.relay и live.node - null.
	checkContract(t, router, s, "POST", "/api/v1/servers", serverJSON(2, map[string]any{"relay_code": nil, "node_code": nil, "state": "disabled"}), http.StatusCreated)
	checkContract(t, router, s, "POST", "/api/v1/servers", serverJSON(1, nil), http.StatusConflict)
	checkContract(t, router, s, "POST", "/api/v1/servers", serverJSON(3, map[string]any{"settings": map[string]string{"nope": "1"}}), http.StatusBadRequest)
	checkContract(t, router, s, "POST", "/api/v1/servers", serverJSON(3, map[string]any{"relay_code": "pr9"}), http.StatusBadRequest)
	checkContract(t, router, s, "GET", "/api/v1/servers", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/servers?engine=mtg&state=test&limit=5&offset=0", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/servers/1", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/servers/42", "", http.StatusNotFound)
	checkContract(t, router, s, "GET", "/api/v1/servers/fields", "", http.StatusOK)
	checkContract(t, router, s, "GET", "/api/v1/servers/fields?engine=telemt", "", http.StatusOK)
	checkContract(t, router, s, "PUT", "/api/v1/servers/1", serverJSON(1, map[string]any{"name": "Нидерланды"}), http.StatusOK)
	checkContract(t, router, s, "PUT", "/api/v1/servers/1", serverJSON(1, map[string]any{"engine": "mtg", "settings": map[string]string{}}), http.StatusBadRequest)
	checkContract(t, router, s, "PUT", "/api/v1/servers/42", serverJSON(42, nil), http.StatusNotFound)
	checkContract(t, router, s, "PUT", "/api/v1/servers", "["+serverJSON(1, nil)+","+serverJSON(4, nil)+"]", http.StatusOK)
	checkContract(t, router, s, "PUT", "/api/v1/servers", "["+serverJSON(5, map[string]any{"endpoints": []map[string]any{{"protocol": "ee", "host": "a.example.com"}}})+"]", http.StatusBadRequest)
	checkContract(t, router, s, "DELETE", "/api/v1/servers/1", "", http.StatusConflict)
	checkContract(t, router, s, "DELETE", "/api/v1/servers/2", "", http.StatusNoContent)
	checkContract(t, router, s, "DELETE", "/api/v1/servers/2", "", http.StatusNotFound)
	checkContractResponse(t, router, s, "/api/v1/servers", "/api/v1/servers?engine=xray", http.StatusBadRequest)
}
