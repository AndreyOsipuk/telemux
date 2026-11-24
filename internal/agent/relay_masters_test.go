package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayMastersEmptyStopsProbesKeepsBootstrap(t *testing.T) {
	var advertise atomic.Bool
	var probes atomic.Int32
	var accepts atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/desired" {
			return
		}
		if r.URL.Query().Get("wait") == "0s" {
			probes.Add(1)
		} else {
			accepts.Store(r.Header.Get("X-Telemux-Accept-Relay-Masters") == "1")
		}
		if advertise.Load() {
			w.Header().Set("X-Telemux-Relay-Masters", base64.RawURLEncoding.EncodeToString([]byte(`{"paths":[]}`)))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	bootstrap := testRelayMasterURL(t, server)
	a, err := New(Config{MasterURLs: []string{bootstrap, bootstrap}, Target: "pr1", Token: "test", Mode: ModeCompare})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a.ProbeMasters(ctx)
	if probes.Load() != 2 {
		t.Fatalf("до ответа реестра должны использоваться два стартовых пути, проб: %d", probes.Load())
	}
	advertise.Store(true)
	for range 2 {
		if _, err := a.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	a.ProbeMasters(ctx)
	if probes.Load() != 2 {
		t.Fatalf("после пустого списка не должно быть новых проб, всего: %d", probes.Load())
	}
	if !accepts.Load() {
		t.Fatal("агент не отправил признак поддержки списка путей")
	}
}

func relayPathHeaderForTest(t *testing.T, paths ...relayPath) string {
	t.Helper()
	raw, err := json.Marshal(relayPathsDocument{Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// testRelaySOCKS открывает локальный SOCKS и подменяет адрес тестовой ноды
// локальным сервером. Проверяем реальный HTTP-транспорт агента через SOCKS.
func testRelayMasterURL(t *testing.T, server *httptest.Server) string {
	t.Helper()
	return "http://192.0.2.10:18480|" + testRelaySOCKS(t, server.Listener.Addr().String())
}

func testRelaySOCKS(t *testing.T, upstream string) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go mapTestRelayConnection(conn, upstream)
		}
	}()
	return "socks5://" + listener.Addr().String()
}

func mapTestRelayConnection(conn net.Conn, upstream string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var greeting [3]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil || greeting != [3]byte{5, 1, 0} {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	var command [10]byte
	if _, err := io.ReadFull(conn, command[:]); err != nil ||
		!bytes.Equal(command[:4], []byte{5, 1, 0, 1}) || command[8] != 72 || command[9] != 48 {
		return
	}
	dest, err := net.DialTimeout("tcp4", upstream, time.Second)
	if err != nil {
		return
	}
	defer dest.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(dest, conn)
		_ = dest.(*net.TCPConn).CloseWrite()
	}()
	_, _ = io.Copy(conn, dest)
}

func TestRelayMastersValidateAdoptPersistAndKeepOnWrongBridge(t *testing.T) {
	var header atomic.Value
	var dsPolls atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/relays":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Error("проверка идентичности моста получила токен")
			}
			_, _ = io.WriteString(w, `{"bridge":"ps1","relays":[]}`)
		case "/v1/desired":
			if got := r.Header.Get("Authorization"); got != "Bearer robot-token" {
				t.Error("управляющий запрос без токена")
			}
			dsPolls.Add(1)
			w.Header().Set("X-Telemux-Relay-Masters", header.Load().(string))
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer bridge.Close()
	path1 := relayPath{Exit: "ps1", Master: "http://192.0.2.41:18480|" + testRelaySOCKS(t, bridge.Listener.Addr().String())}
	path2 := relayPath{Exit: "ps2", Master: "http://192.0.2.42:18480|" + testRelaySOCKS(t, bridge.Listener.Addr().String())}
	header.Store(relayPathHeaderForTest(t, path1))
	bootstrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Telemux-Relay-Masters", header.Load().(string))
		_, _ = io.WriteString(w, `{"revision":1,"resources":[]}`)
	}))
	defer bootstrap.Close()
	cache := filepath.Join(t.TempDir(), "paths.json")
	bootstrapURL := testRelayMasterURL(t, bootstrap)
	cfg := Config{MasterURLs: []string{bootstrapURL}, RelayPathsCache: cache, Target: "pr1", Token: "robot-token", Mode: ModeCompare}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if rev, err := a.RunOnce(ctx); err != nil || rev != 1 {
		t.Fatalf("стартовый запрос: rev=%d, err=%v", rev, err)
	}
	a.ProbeMasters(ctx)
	if dsPolls.Load() != 2 {
		t.Fatalf("ожидались проверка мастера при приёмке и одна периодическая проба, получено %d", dsPolls.Load())
	}
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("права кэша %o", info.Mode().Perm())
	}
	header.Store(relayPathHeaderForTest(t, path1, path2))
	if _, err := a.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if paths := a.probeMasters(); len(paths) != 1 || paths[0].entry != path1.Master {
		t.Fatalf("ответ поверх неверного моста изменил список: %v", paths)
	}
	bootstrap.Close()
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RunOnce(ctx); err != nil {
		t.Fatalf("кэш не пережил перезапуск при недоступном стартовом адресе: %v", err)
	}
	if paths := restarted.probeMasters(); len(paths) != 1 || paths[0].entry != path1.Master {
		t.Fatalf("неверный кэш: %v", paths)
	}
	cfg.Target = "pr2"
	foreign, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if masters := foreign.controlMasters(); len(masters) != 1 || masters[0].entry != bootstrapURL {
		t.Fatal("агент загрузил кэш другой машины")
	}
}

func TestRelayMastersRejectUnsafeOrDuplicateEntries(t *testing.T) {
	a, err := New(Config{MasterURLs: []string{"http://bootstrap.example:8090"}, Target: "pr1", Token: "test", Mode: ModeCompare})
	if err != nil {
		t.Fatal(err)
	}
	const proxy = "socks5://127.0.0.1:10874"
	const good = "http://192.0.2.1:18480|" + proxy
	cases := [][]relayPath{
		nil,
		{{Exit: "ps1", Master: "http://192.0.2.1:18480"}},
		{{Exit: "ps1", Master: "https://192.0.2.1:18480|" + proxy}},
		{{Exit: "ps1", Master: "http://192.0.2.1:18481|" + proxy}},
		{{Exit: "ps1", Master: "http://192.0.2.1:18480|" + strings.Replace(proxy, "127.0.0.1", "192.0.2.9", 1)}},
		{{Exit: "ps1", Master: good}, {Exit: "ps1", Master: strings.Replace(good, "10874", "10875", 1)}},
		{{Exit: "ps1", Master: good}, {Exit: "ps2", Master: strings.Replace(good, "10874", "010874", 1)}},
	}
	for i, endpoints := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := a.parseRelayPaths(endpoints); err == nil {
				t.Fatal("ожидался отказ")
			}
		})
	}
}

func TestRelayMastersKeepsCurrentPriority(t *testing.T) {
	ps1 := relayPath{Exit: "ps1", Master: "one"}
	ps2 := relayPath{Exit: "ps2", Master: "two"}
	ps3 := relayPath{Exit: "ps3", Master: "three"}
	got := orderRelayPaths([]relayPath{ps1, ps2, ps3}, []master{{entry: "two"}, {entry: "one"}})
	if !slices.Equal(got, []relayPath{ps2, ps1, ps3}) {
		t.Fatalf("приоритет путей изменился: %v", got)
	}
}

func TestRelayMastersFailedManagedPathsUseBootstrapOnlyForControl(t *testing.T) {
	var fallbackPolls atomic.Int32
	var fallbackProbes atomic.Int32
	bootstrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/desired" {
			if r.URL.Query().Get("wait") == "0s" {
				fallbackProbes.Add(1)
			} else {
				fallbackPolls.Add(1)
			}
			w.Header().Set("X-Telemux-Relay-Masters", base64.RawURLEncoding.EncodeToString([]byte(`{"paths":[]}`)))
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer bootstrap.Close()
	managed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer managed.Close()

	cache := filepath.Join(t.TempDir(), "paths.json")
	active := []relayPath{{Exit: "ps1", Master: testRelayMasterURL(t, managed)}}
	if err := saveRelayPathsCache(cache, relayPathsCache{Target: "pr1", relayPathsDocument: relayPathsDocument{Paths: active}}); err != nil {
		t.Fatal(err)
	}
	bootstrapURL := testRelayMasterURL(t, bootstrap)
	cfg := Config{MasterURLs: []string{bootstrapURL}, RelayPathsCache: cache, Target: "pr1", Token: "test", Mode: ModeCompare}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a.ProbeMasters(ctx)
	if _, err := a.RunOnce(ctx); err != nil {
		t.Fatalf("стартовый маршрут не восстановил связь: %v", err)
	}
	a.ProbeMasters(ctx)
	if fallbackPolls.Load() != 1 || fallbackProbes.Load() != 0 {
		t.Fatalf("вызовы стартового маршрута: опросы=%d проверки=%d", fallbackPolls.Load(), fallbackProbes.Load())
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.probeMasters()) != 0 {
		t.Fatal("пустой реестр не пережил перезапуск")
	}
}

func TestRelayMastersRejectBridgeWithoutAuthorizedMaster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/relays" {
			_, _ = io.WriteString(w, `{"bridge":"ps1"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	masterURL := testRelayMasterURL(t, server)
	a, err := New(Config{MasterURLs: []string{masterURL}, Target: "pr1", Token: "test", Mode: ModeCompare})
	if err != nil {
		t.Fatal(err)
	}
	err = a.updateRelayPaths(context.Background(), relayPathHeaderForTest(t, relayPath{Exit: "ps1", Master: masterURL}))
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || a.relayPaths != nil {
		t.Fatalf("релей принял путь без доступа к мастеру: %v", err)
	}
}
