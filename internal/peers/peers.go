// Package peers - список голосующих машин управления и их опрос по HTTP.
//
// Один из двух каналов кворума (второй - streaming-реплики в pg_stat_replication):
// telemux пингует соседние telemux и считает соседа видимым, если тот ответил 200
// и назвал ожидаемый код. Ответ заодно сообщает роль соседа - так реплика узнаёт,
// кто мастер, и подсказывает клиентам.
package peers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/role"
)

// PingPath - путь пинга соседа (под Bearer cluster-secret).
const PingPath = "/api/cluster/peer"

// Ping - тело ответа на пинг.
type Ping struct {
	Code string    `json:"code"`
	Role role.Role `json:"role"`
}

// Parse разбирает список соседей вида "ps2=https://a:8080,ps3=https://b:8080".
func Parse(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		code, url, ok := strings.Cut(part, "=")
		code, url = strings.TrimSpace(code), strings.TrimRight(strings.TrimSpace(url), "/")
		if !ok || code == "" || url == "" {
			return nil, fmt.Errorf("сосед %q: нужен формат code=url", part)
		}
		if _, dup := out[code]; dup {
			return nil, fmt.Errorf("сосед %q указан дважды", code)
		}
		out[code] = url
	}
	return out, nil
}

// Codes - отсортированные коды соседей.
func Codes(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Prober опрашивает соседей параллельно. Реализует role.PeerProber.
type Prober struct {
	Peers  map[string]string // code → базовый URL telemux
	Secret string            // cluster-secret (Bearer)
	HTTP   *http.Client      // nil → таймаут 1.5 с
}

// Probe - по одному результату на соседа, в порядке кодов.
func (p *Prober) Probe(ctx context.Context) []role.PeerStatus {
	hc := p.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 1500 * time.Millisecond}
	}
	codes := Codes(p.Peers)
	out := make([]role.PeerStatus, len(codes))
	var wg sync.WaitGroup
	for i, code := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = p.probeOne(ctx, hc, code, p.Peers[code])
		}()
	}
	wg.Wait()
	return out
}

func (p *Prober) probeOne(ctx context.Context, hc *http.Client, code, base string) role.PeerStatus {
	res := role.PeerStatus{Code: code}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+PingPath, nil)
	if err != nil {
		return res
	}
	if p.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+p.Secret)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return res
	}
	defer func() {
		// Дочитываем тело, чтобы keep-alive соединение переиспользовалось между раундами.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return res
	}
	var ping Ping
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&ping); err != nil {
		return res
	}
	// Отвечает не тот узел (перепутан адрес, балансировщик) - канал не засчитываем.
	if ping.Code != code {
		return res
	}
	res.OK, res.Role = true, ping.Role
	return res
}
