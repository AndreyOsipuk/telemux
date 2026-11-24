package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/agentjoin"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

const (
	relayPathsAcceptHeader = "X-Telemux-Accept-Relay-Masters"
	relayPathsHeader       = "X-Telemux-Relay-Masters"
	maxRelayPaths          = store.MaxRelayMasterPaths
	maxRelayPathsBytes     = 16 << 10
)

// DefaultRelayPathsCache - каталог под StateDirectory=telemux-agent.
const DefaultRelayPathsCache = "/var/lib/telemux-agent/relay-paths.json"

type relayPath struct {
	Exit   string `json:"exit"`
	Master string `json:"master"`
}

type relayPathsDocument struct {
	Paths []relayPath `json:"paths"`
}

type relayPathsCache struct {
	Target string `json:"target"`
	relayPathsDocument
}

// controlMasters возвращает пути для long-poll и runtime. Сначала действующие
// пути из БД, затем стартовые для восстановления связи с мастером.
func (a *Agent) controlMasters() []master {
	a.relayPathsMu.RLock()
	defer a.relayPathsMu.RUnlock()
	if len(a.relayMasters) == 0 {
		return a.masters
	}
	out := slices.Clone(a.relayMasters)
	for _, bootstrap := range a.masters {
		if !slices.ContainsFunc(out, func(existing master) bool { return existing.entry == bootstrap.entry }) {
			out = append(out, bootstrap)
		}
	}
	return out
}

// stackMasterURLs передаёт текущий список отдельному процессу stack-apply.
// Без списка из БД процесс по-прежнему читает штатное локальное окружение.
func (a *Agent) stackMasterURLs() []string {
	a.relayPathsMu.RLock()
	managed := a.relayPaths != nil
	a.relayPathsMu.RUnlock()
	if !managed {
		return nil
	}
	paths := a.controlMasters()
	out := make([]string, 0, len(paths))
	for _, m := range paths {
		out = append(out, m.entry)
	}
	return out
}

func (a *Agent) probeMasters() []master {
	a.relayPathsMu.RLock()
	defer a.relayPathsMu.RUnlock()
	if a.relayPaths != nil {
		return a.relayMasters
	}
	if len(a.masters) > 1 {
		return a.masters
	}
	return nil
}

func (a *Agent) parseRelayPaths(paths []relayPath) ([]master, error) {
	if paths == nil || len(paths) > maxRelayPaths {
		return nil, fmt.Errorf("ожидался массив из 0–%d путей", maxRelayPaths)
	}
	exits := make(map[string]bool, len(paths))
	proxies := make(map[string]bool, len(paths))
	masters := make([]master, 0, len(paths))
	for _, p := range paths {
		base, proxy, ok := strings.Cut(p.Master, "|")
		if !ok || !agentjoin.ValidCode(p.Exit) || exits[p.Exit] {
			return nil, errors.New("некорректный или повторный код выхода либо путь без SOCKS")
		}
		route := agentjoin.Route{Base: base, Proxy: proxy}
		if err := route.Validate(); err != nil {
			return nil, fmt.Errorf("выход %s: %w", p.Exit, err)
		}
		// Ведущие нули в порту не создают второй независимый SOCKS-путь.
		port := strings.TrimLeft(strings.TrimPrefix(proxy, "socks5://127.0.0.1:"), "0")
		if proxies[port] {
			return nil, fmt.Errorf("выход %s: SOCKS уже используется другим выходом", p.Exit)
		}
		exits[p.Exit], proxies[port] = true, true
		m, err := parseMaster(p.Master, a.cfg.Wait+15*time.Second, a.cfg.MasterPin)
		if err != nil {
			return nil, err
		}
		// Мост не должен перенаправлять запрос с токеном на другой сервер.
		m.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		masters = append(masters, m)
	}
	return masters, nil
}

// loadRelayPaths читает последний принятый список своего релея без сетевой
// проверки. Временная недоступность моста не должна мешать загрузке кэша.
func (a *Agent) loadRelayPaths() {
	if a.cfg.RelayPathsCache == "" {
		return
	}
	if err := a.readRelayPathsCache(); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.cfg.Log.Warn("агент: кэш путей релея не прочитан, использую стартовый список", "err", err)
	}
}

func (a *Agent) readRelayPathsCache() error {
	f, err := os.Open(a.cfg.RelayPathsCache)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRelayPathsBytes+1))
	if err != nil {
		return err
	}
	var cache relayPathsCache
	if len(raw) > maxRelayPathsBytes {
		return errors.New("слишком большой кэш путей")
	}
	if err := json.Unmarshal(raw, &cache); err != nil {
		return err
	}
	if cache.Target != a.cfg.Target {
		return errors.New("кэш путей относится к другой машине")
	}
	masters, err := a.parseRelayPaths(cache.Paths)
	if err != nil {
		return err
	}
	a.relayPaths, a.relayMasters = cache.Paths, masters
	return nil
}

func hasBootstrapRelayPath(masters []master) bool {
	for _, m := range masters {
		_, proxy, ok := strings.Cut(m.entry, "|")
		if ok && agentjoin.ValidBridgeSocks(proxy) == nil {
			return true
		}
	}
	return false
}

func (a *Agent) acceptRelayPaths(ctx context.Context, header string) {
	if !a.relayPathsEnabled || header == "" {
		return
	}
	if err := a.updateRelayPaths(ctx, header); err != nil {
		a.cfg.Log.Warn("агент: новый список путей отклонён, использую прежний", "err", err)
	}
}

func (a *Agent) updateRelayPaths(ctx context.Context, header string) error {
	if len(header) > base64.RawURLEncoding.EncodedLen(maxRelayPathsBytes) {
		return errors.New("слишком большой список путей")
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return fmt.Errorf("кодировка списка путей: %w", err)
	}
	if len(raw) > maxRelayPathsBytes {
		return errors.New("слишком большой список путей")
	}
	var doc relayPathsDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("список путей: %w", err)
	}
	// Обновления приходят только в основном цикле long-poll.
	a.relayPathsMu.RLock()
	current := a.relayPaths
	a.relayPathsMu.RUnlock()
	preferred := a.controlMasters()
	paths := orderRelayPaths(doc.Paths, preferred)
	if current != nil && paths != nil && slices.Equal(current, paths) {
		return nil
	}
	masters, err := a.parseRelayPaths(paths)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			for _, m := range masters {
				m.client.CloseIdleConnections()
			}
		}
	}()
	if err := a.verifyRelayPaths(ctx, current, paths, masters); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if file := a.cfg.RelayPathsCache; file != "" {
		if err := saveRelayPathsCache(file, relayPathsCache{Target: a.cfg.Target, relayPathsDocument: relayPathsDocument{Paths: paths}}); err != nil {
			return fmt.Errorf("сохранение путей: %w", err)
		}
	}
	a.relayPathsMu.Lock()
	obsolete := a.relayMasters
	a.relayPaths, a.relayMasters = paths, masters
	a.relayPathsMu.Unlock()
	accepted = true
	for _, m := range obsolete {
		m.client.CloseIdleConnections()
	}
	a.cfg.Log.Info("агент: обновлён список путей релея", "paths", len(paths))
	return nil
}

// orderRelayPaths сохраняет приоритет уже действующих адресов. Сортировка
// реестра по коду не должна менять выбранный для long-poll мост.
func orderRelayPaths(paths []relayPath, preferred []master) []relayPath {
	ordered := slices.Clone(paths)
	ranks := make(map[string]int, len(preferred))
	for i, m := range preferred {
		ranks[m.entry] = len(preferred) - i
	}
	slices.SortStableFunc(ordered, func(a, b relayPath) int { return ranks[b.Master] - ranks[a.Master] })
	return ordered
}

func (a *Agent) verifyRelayPaths(ctx context.Context, current, paths []relayPath, masters []master) error {
	verified := make(map[relayPath]bool, len(current))
	for _, path := range current {
		verified[path] = true
	}
	// Независимые проверки параллельны. Ошибка одного нового пути отклоняет
	// всё обновление, сохраняя уже принятый список и кэш.
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, path := range paths {
		if verified[path] {
			continue
		}
		wg.Go(func() { errs[i] = a.verifyRelayPath(ctx, path.Exit, masters[i]) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (a *Agent) verifyRelayPath(ctx context.Context, exit string, m master) error {
	ctx, cancel := context.WithTimeout(ctx, a.probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/v1/relays", nil)
	if err != nil {
		return fmt.Errorf("выход %s: %w", exit, err)
	}
	resp, err := m.client.Do(req) // ключ агента мосту для проверки кода не нужен
	if err != nil {
		return fmt.Errorf("выход %s: %w", exit, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("выход %s: мост ответил HTTP %d", exit, resp.StatusCode)
	}
	var body struct {
		Bridge string `json:"bridge"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRelayPathsBytes)).Decode(&body); err != nil {
		return fmt.Errorf("выход %s: нечитаемая сводка моста: %w", exit, err)
	}
	if body.Bridge != exit {
		return fmt.Errorf("выход %s: код моста не совпал", exit)
	}
	// Мост может отвечать локально, хотя путь к мастеру сломан или ведёт в
	// другой кластер. Проверяем авторизованный ответ без выгрузки ресурсов.
	q := url.Values{"target": {a.cfg.Target}, "rev": {strconv.FormatInt(math.MaxInt64, 10)}, "wait": {"0s"}}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/v1/desired?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("выход %s: %w", exit, err)
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	masterResponse, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("выход %s: мастер недоступен: %w", exit, err)
	}
	defer masterResponse.Body.Close()
	if masterResponse.StatusCode != http.StatusOK && masterResponse.StatusCode != http.StatusNotModified {
		return fmt.Errorf("выход %s: мастер ответил HTTP %d", exit, masterResponse.StatusCode)
	}
	return nil
}

func saveRelayPathsCache(path string, cache relayPathsCache) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".relay-paths-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(cache); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
