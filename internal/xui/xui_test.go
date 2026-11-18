package xui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

func own(email, secret string) Client {
	return Client{Email: email, Secret: secret, SubID: "mtpx-" + strings.Repeat("a", 24), Enable: true}
}

// План: добавить новых, обновить изменившихся, своих лишних удалить, чужих отвязать.
func TestPlanSync(t *testing.T) {
	cur := []Client{
		own("alice_1_ps7", "aa"),
		own("bob_2_ps7", "bb"),
		own("old_3_ps7", "cc"),
		{Email: "mtg_tok_9", Secret: "dd", SubID: "vpn-sub"}, // клиент VPN-бота
		own("x_4_ps8", "ee"),                                 // свой префикс, но другая нода
	}
	want := []Client{own("alice_1_ps7", "aa"), own("bob_2_ps7", "b2"), own("new_5_ps7", "ff")}
	p := PlanSync(cur, want, "ps7")
	if len(p.Add) != 1 || p.Add[0].Email != "new_5_ps7" {
		t.Fatalf("add: %+v", p.Add)
	}
	if len(p.Update) != 1 || p.Update[0].Email != "bob_2_ps7" {
		t.Fatalf("update: %+v", p.Update)
	}
	if strings.Join(p.Remove, ",") != "old_3_ps7" {
		t.Fatalf("remove: %v", p.Remove)
	}
	sort.Strings(p.Detach)
	if strings.Join(p.Detach, ",") != "mtg_tok_9,x_4_ps8" {
		t.Fatalf("detach: %v", p.Detach)
	}
	if p.Empty() {
		t.Fatal("план не пустой")
	}
	if !PlanSync(want, want, "ps7").Empty() {
		t.Fatal("совпадающие наборы дают пустой план")
	}
}

func TestIsMassRemoval(t *testing.T) {
	p := Plan{Remove: make([]string, 10)}
	if !p.IsMassRemoval(15) {
		t.Fatal("10 из 15 - массовое удаление")
	}
	if p.IsMassRemoval(30) {
		t.Fatal("10 из 30 - не массовое")
	}
	if (Plan{Remove: make([]string, 5)}).IsMassRemoval(6) {
		t.Fatal("меньше порога - не массовое")
	}
}

// fakePanel - мастер-панель 3x-ui: один MTProto-вход.
type fakePanel struct {
	mu      sync.Mutex
	inbound Inbound
	token   string
	calls   []string
}

func (f *fakePanel) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		ok := func(obj any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "msg": "", "obj": obj}) }
		switch {
		case r.URL.Path == "/panel/api/inbounds/get/7":
			settings := map[string]any{"clients": f.inbound.Clients}
			ok(map[string]any{"id": 7, "protocol": f.inbound.Protocol, "port": f.inbound.Port, "remark": f.inbound.Remark,
				"nodeId": f.inbound.NodeID, "settings": settings})
		case r.URL.Path == "/panel/api/clients/bulkCreate":
			var items []struct {
				Client     Client `json:"client"`
				InboundIDs []int  `json:"inboundIds"`
			}
			_ = json.Unmarshal(body, &items)
			for _, it := range items {
				f.inbound.Clients = append(f.inbound.Clients, it.Client)
			}
			ok(map[string]any{"created": len(items), "skipped": []any{}})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/update/"):
			var c Client
			_ = json.Unmarshal(body, &c)
			email := strings.TrimPrefix(r.URL.Path, "/panel/api/clients/update/")
			for i := range f.inbound.Clients {
				if f.inbound.Clients[i].Email == email {
					f.inbound.Clients[i] = c
				}
			}
			ok(nil)
		case r.URL.Path == "/panel/api/clients/bulkDel" || strings.HasSuffix(r.URL.Path, "/detach"):
			var del struct {
				Emails []string `json:"emails"`
			}
			_ = json.Unmarshal(body, &del)
			if strings.HasSuffix(r.URL.Path, "/detach") {
				del.Emails = []string{strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/panel/api/clients/"), "/detach")}
			}
			keep := f.inbound.Clients[:0]
			for _, c := range f.inbound.Clients {
				drop := false
				for _, e := range del.Emails {
					drop = drop || c.Email == e
				}
				if !drop {
					keep = append(keep, c)
				}
			}
			f.inbound.Clients = keep
			ok(map[string]any{"deleted": len(del.Emails), "skipped": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newPanel(clients ...Client) *fakePanel {
	nodeID := 3
	return &fakePanel{token: "tok", inbound: Inbound{ID: 7, Protocol: "mtproto", Port: 8767, Remark: "ps7 [mtproxy-bot]", NodeID: &nodeID, Clients: clients}}
}

func target() Target { return Target{Code: "ps7", InboundID: 7, NodeID: 3, Port: 8767} }

// Полная сверка: после неё во входе ровно желаемый набор.
func TestSyncer_Reconciles(t *testing.T) {
	fp := newPanel(own("alice_1_ps7", "aa"), own("old_3_ps7", "cc"), Client{Email: "mtg_tok_9", Secret: "dd", SubID: "vpn"})
	api := NewAPI(fp.server(t).URL, "tok", nil)
	want := []Client{own("alice_1_ps7", "a2"), own("new_5_ps7", "ff")}
	res := Sync(context.Background(), api, target(), want, false)
	if res.Error != "" {
		t.Fatalf("сверка с ошибкой: %s", res.Error)
	}
	got := map[string]string{}
	for _, c := range fp.inbound.Clients {
		got[c.Email] = c.Secret
	}
	if len(got) != 2 || got["alice_1_ps7"] != "a2" || got["new_5_ps7"] != "ff" {
		t.Fatalf("во входе не желаемый набор: %v", got)
	}
}

// Совпадает - ни одного запроса на запись.
func TestSyncer_NoWritesWhenInSync(t *testing.T) {
	want := []Client{own("alice_1_ps7", "aa")}
	fp := newPanel(want...)
	res := Sync(context.Background(), NewAPI(fp.server(t).URL, "tok", nil), target(), want, false)
	if res.Error != "" || res.Changed {
		t.Fatalf("ждали без изменений: %+v", res)
	}
	for _, c := range fp.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("при совпадении был запрос на запись: %s", c)
		}
	}
}

// Вход не тот (не mtproto, не та нода, не тот порт, без метки) - отказ без записи.
func TestSyncer_RejectsWrongInbound(t *testing.T) {
	for name, mutate := range map[string]func(*fakePanel){
		"protocol": func(f *fakePanel) { f.inbound.Protocol = "vless" },
		"port":     func(f *fakePanel) { f.inbound.Port = 443 },
		"node":     func(f *fakePanel) { n := 9; f.inbound.NodeID = &n },
		"mark":     func(f *fakePanel) { f.inbound.Remark = "ps7" },
	} {
		fp := newPanel()
		mutate(fp)
		res := Sync(context.Background(), NewAPI(fp.server(t).URL, "tok", nil), target(), []Client{own("a_1_ps7", "aa")}, false)
		if res.Error == "" {
			t.Fatalf("%s: чужой вход принят", name)
		}
		if len(fp.calls) != 1 {
			t.Fatalf("%s: при чужом входе были запросы на запись: %v", name, fp.calls)
		}
	}
}

// Массовое удаление - отказ без записи, пока не разрешено явно.
func TestSyncer_MassRemovalGuard(t *testing.T) {
	var cur []Client
	for i := 0; i < 12; i++ {
		cur = append(cur, own("u_"+string(rune('a'+i))+"_ps7", "aa"))
	}
	fp := newPanel(cur...)
	api := NewAPI(fp.server(t).URL, "tok", nil)
	res := Sync(context.Background(), api, target(), []Client{own("keep_1_ps7", "aa")}, false)
	if res.Error == "" || len(fp.inbound.Clients) != 12 {
		t.Fatalf("массовое удаление должно быть остановлено: %+v, клиентов %d", res, len(fp.inbound.Clients))
	}
	if res := Sync(context.Background(), api, target(), []Client{own("keep_1_ps7", "aa")}, true); res.Error != "" || len(fp.inbound.Clients) != 1 {
		t.Fatalf("с разрешением удаление должно пройти: %+v", res)
	}
}

// Неверный токен - ошибка, токен не попадает в текст ошибки.
func TestAPI_BadToken(t *testing.T) {
	fp := newPanel()
	res := Sync(context.Background(), NewAPI(fp.server(t).URL, "secret-token-xyz", nil), target(), nil, false)
	if res.Error == "" || strings.Contains(res.Error, "secret-token-xyz") {
		t.Fatalf("ждали ошибку без токена в тексте: %q", res.Error)
	}
}

// Отпечаток набора - как у mtg-сайдкара: имена = email, только включённые.
func TestDigest(t *testing.T) {
	a := Digest([]Client{own("b_2_ps7", "BB"), own("a_1_ps7", "aa"), {Email: "off_ps7", Secret: "cc", Enable: false}})
	b := Digest([]Client{own("a_1_ps7", "aa"), own("b_2_ps7", "bb")})
	if a != b {
		t.Fatal("отпечаток зависит от порядка, регистра или выключенных клиентов")
	}
}

// Закрепление сертификата: верный отпечаток пускает, неверный - нет (токен не уходит).
func TestPinnedTLS(t *testing.T) {
	fp := newPanel(own("a_1_ps7", "aa"))
	srv := httptest.NewUnstartedServer(fp.server(t).Config.Handler)
	srv.StartTLS()
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	good := hex.EncodeToString(sum[:])
	if _, err := NewAPI(srv.URL, "tok", PinnedTLS(good)).GetInbound(context.Background(), 7); err != nil {
		t.Fatalf("верный отпечаток не пустил: %v", err)
	}
	fp.calls = nil
	if _, err := NewAPI(srv.URL, "tok", PinnedTLS(strings.Repeat("0", 64))).GetInbound(context.Background(), 7); err == nil {
		t.Fatal("неверный отпечаток пустил")
	}
	if len(fp.calls) != 0 {
		t.Fatal("при неверном сертификате запрос с токеном дошёл до панели")
	}
}
