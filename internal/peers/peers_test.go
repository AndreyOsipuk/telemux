package peers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/role"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"два соседа, пробелы и завершающий слеш", "ps2=http://a:8080/, ps3=http://b",
			map[string]string{"ps2": "http://a:8080", "ps3": "http://b"}, false},
		{"пустая строка - пустая map", "", map[string]string{}, false},
		{"только запятые и пробелы - пустая map", " , ,", map[string]string{}, false},
		{"несколько слешей в конце обрезаются", "ps2=http://a///", map[string]string{"ps2": "http://a"}, false},
		{"без = - ошибка", "ps2", nil, true},
		{"дубль кода - ошибка", "ps2=http://a,ps2=http://b", nil, true},
		{"пустой код - ошибка", "=http://x", nil, true},
		{"пустой url - ошибка", "ps2=", nil, true},
		{"url из одного слеша - ошибка", "ps2=/", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q): ожидали ошибку, получили %v", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): неожиданная ошибка: %v", tt.in, err)
			}
			if got == nil {
				t.Fatalf("Parse(%q): вернулась nil map", tt.in)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q) = %v, ожидали %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCodes_Sorted(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want []string
	}{
		{"сортировка", map[string]string{"ps3": "c", "ps1": "a", "ps2": "b"}, []string{"ps1", "ps2", "ps3"}},
		{"пустая map", map[string]string{}, []string{}},
		{"nil map", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Codes(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Codes = %v, ожидали %v", got, tt.want)
			}
		})
	}
}

// pingServer - фейковый сосед telemux, отвечающий на PingPath.
// Записывает в *gotAuth заголовок Authorization последнего запроса на PingPath.
func pingServer(t *testing.T, code int, ping Ping, mu *sync.Mutex, gotAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PingPath || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if gotAuth != nil {
			mu.Lock()
			*gotAuth = r.Header.Get("Authorization")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(ping)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedURL возвращает адрес уже закрытого сервера (сосед недоступен).
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func TestProber_Probe_OKAndAuth(t *testing.T) {
	// Arrange
	var mu sync.Mutex
	var auth string
	srv := pingServer(t, http.StatusOK, Ping{Code: "ps2", Role: role.Master}, &mu, &auth)
	p := &Prober{Peers: map[string]string{"ps2": srv.URL}, Secret: "s3cr3t"}

	// Act
	res := p.Probe(context.Background())

	// Assert
	want := []role.PeerStatus{{Code: "ps2", OK: true, Role: role.Master}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("Probe = %+v, ожидали %+v", res, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q, ожидали %q", auth, "Bearer s3cr3t")
	}
}

func TestProber_Probe_NoSecretNoHeader(t *testing.T) {
	var mu sync.Mutex
	auth := "не задан"
	srv := pingServer(t, http.StatusOK, Ping{Code: "ps2", Role: role.Replica}, &mu, &auth)
	p := &Prober{Peers: map[string]string{"ps2": srv.URL}}

	res := p.Probe(context.Background())

	if len(res) != 1 || !res[0].OK || res[0].Role != role.Replica {
		t.Fatalf("Probe = %+v, ожидали OK=true, Role=replica", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "" {
		t.Fatalf("без секрета Authorization не должен отправляться, получили %q", auth)
	}
}

func TestProber_Probe_Failures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string // возвращает базовый URL соседа ps3
	}{
		{"401 - не засчитываем", func(t *testing.T) string {
			return pingServer(t, http.StatusUnauthorized, Ping{Code: "ps3", Role: role.Master}, nil, nil).URL
		}},
		{"500 - не засчитываем", func(t *testing.T) string {
			return pingServer(t, http.StatusInternalServerError, Ping{Code: "ps3", Role: role.Master}, nil, nil).URL
		}},
		{"чужой код ps9 вместо ps3 - не засчитываем", func(t *testing.T) string {
			return pingServer(t, http.StatusOK, Ping{Code: "ps9", Role: role.Master}, nil, nil).URL
		}},
		{"пустой код - не засчитываем", func(t *testing.T) string {
			return pingServer(t, http.StatusOK, Ping{Role: role.Master}, nil, nil).URL
		}},
		{"200 с не-JSON телом - не засчитываем", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("<html>ok</html>"))
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}},
		{"сосед недоступен - не засчитываем", closedURL},
		{"битый URL - не засчитываем", func(t *testing.T) string { return "://bad url" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Prober{
				Peers:  map[string]string{"ps3": tt.setup(t)},
				Secret: "s",
				HTTP:   &http.Client{Timeout: time.Second},
			}

			res := p.Probe(context.Background())

			want := []role.PeerStatus{{Code: "ps3"}}
			if !reflect.DeepEqual(res, want) {
				t.Fatalf("Probe = %+v, ожидали %+v", res, want)
			}
		})
	}
}

func TestProber_Probe_HangingPeerTimeout(t *testing.T) {
	// Arrange: сосед зависает, пока клиент не оборвёт соединение (или тест не завершится).
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// Порядок cleanup обратный: сначала отпускаем хендлер, потом закрываем сервер.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	p := &Prober{
		Peers:  map[string]string{"ps2": srv.URL},
		Secret: "s",
		HTTP:   &http.Client{Timeout: 100 * time.Millisecond},
	}

	// Act
	start := time.Now()
	res := p.Probe(context.Background())
	elapsed := time.Since(start)

	// Assert
	if len(res) != 1 || res[0].OK {
		t.Fatalf("Probe = %+v, ожидали OK=false для зависшего соседа", res)
	}
	if elapsed > time.Second {
		t.Fatalf("Probe вернулся за %s - таймаут клиента не сработал", elapsed)
	}
}

func TestProber_Probe_ParallelAndOrdered(t *testing.T) {
	// Arrange: ps1 - доступен, ps2 - зависает, ps3 - чужой код, ps4 - недоступен.
	// Проба зависшего не должна задерживать остальных (опрос параллельный),
	// а результаты идут в порядке кодов, независимо от порядка ответов.
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })

	ok := pingServer(t, http.StatusOK, Ping{Code: "ps1", Role: role.Replica}, nil, nil)
	alien := pingServer(t, http.StatusOK, Ping{Code: "ps9", Role: role.Master}, nil, nil)

	p := &Prober{
		Peers: map[string]string{
			"ps4": closedURL(t),
			"ps3": alien.URL,
			"ps2": hang.URL,
			"ps1": ok.URL,
		},
		Secret: "s",
		HTTP:   &http.Client{Timeout: 200 * time.Millisecond},
	}

	// Act
	start := time.Now()
	res := p.Probe(context.Background())
	elapsed := time.Since(start)

	// Assert
	want := []role.PeerStatus{
		{Code: "ps1", OK: true, Role: role.Replica},
		{Code: "ps2"},
		{Code: "ps3"},
		{Code: "ps4"},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("Probe = %+v, ожидали %+v", res, want)
	}
	if elapsed > time.Second {
		t.Fatalf("Probe занял %s - опрос, похоже, не параллельный", elapsed)
	}
}

func TestProber_Probe_ContextCanceled(t *testing.T) {
	srv := pingServer(t, http.StatusOK, Ping{Code: "ps2", Role: role.Master}, nil, nil)
	p := &Prober{Peers: map[string]string{"ps2": srv.URL}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := p.Probe(ctx)

	if len(res) != 1 || res[0].OK {
		t.Fatalf("при отменённом контексте ожидали OK=false, получили %+v", res)
	}
}

func TestProber_Probe_NoPeers(t *testing.T) {
	res := (&Prober{}).Probe(context.Background())
	if len(res) != 0 {
		t.Fatalf("без соседей ожидали пустой результат, получили %+v", res)
	}
}
