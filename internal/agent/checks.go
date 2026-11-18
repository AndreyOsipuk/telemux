package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

// Check - локальная проверка машины, которую агент кладёт в отчёт: например,
// жив ли мост на машине-выходе. Проверка пройдена, если адрес ответил 2xx.
type Check struct {
	Name string
	URL  string
}

const checkTimeout = 5 * time.Second

// ParseChecks разбирает "имя=http://адрес,имя2=http://адрес2".
func ParseChecks(s string) ([]Check, error) {
	var out []Check
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, raw, ok := strings.Cut(part, "=")
		name, raw = strings.TrimSpace(name), strings.TrimSpace(raw)
		u, err := url.Parse(raw)
		if !ok || name == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("проверка %q: нужен вид имя=http://host:port/путь", part)
		}
		out = append(out, Check{Name: name, URL: raw})
	}
	return out, nil
}

// runChecks выполняет проверки по очереди; каждая - не дольше checkTimeout.
func runChecks(ctx context.Context, checks []Check) []store.ObservedCheck {
	if len(checks) == 0 {
		return nil
	}
	client := &http.Client{Timeout: checkTimeout}
	out := make([]store.ObservedCheck, 0, len(checks))
	for _, c := range checks {
		res := store.ObservedCheck{Name: c.Name, OK: true}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
		if err == nil {
			var resp *http.Response
			resp, err = client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				if resp.StatusCode < 200 || resp.StatusCode > 299 {
					err = fmt.Errorf("ответ %d", resp.StatusCode)
				}
			}
		}
		if err != nil {
			res.OK, res.Error = false, err.Error()
		}
		out = append(out, res)
	}
	return out
}
