// Package patroni - клиент REST API локального Patroni (только чтение).
//
// telemux не управляет PostgreSQL: выборы, промоут, демоут и pg_rewind делает
// Patroni. Отсюда telemux берёт одно из условий мастера - «Patroni считает этот
// узел лидером» - и таймауты для проверки инварианта fencing.
package patroni

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client - клиент REST API Patroni (обычно http://127.0.0.1:8008).
type Client struct {
	base string
	http *http.Client
}

// New создаёт клиент. hc nil → http.Client с таймаутом 3 с.
func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc}
}

// Status - нужная часть ответа GET /patroni.
type Status struct {
	State    string `json:"state"`
	Role     string `json:"role"`
	Timeline int    `json:"timeline"`
	Patroni  struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	} `json:"patroni"`
}

// IsLeader - узел держит лидерство и PG работает как primary.
// Patroni до 3.x называл роль "master", с 3.x - "primary". standby_leader
// (лидер standby-кластера) пишущим не является.
func (s Status) IsLeader() bool {
	return (s.Role == "primary" || s.Role == "master") && s.State == "running"
}

// Status - GET /patroni. Patroni отвечает 503 с тем же телом, если узел не
// здоров; тело разбираем при любом коде.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	if err := c.getJSON(ctx, "/patroni", &st); err != nil {
		return Status{}, err
	}
	if st.Role == "" {
		return Status{}, fmt.Errorf("patroni: в ответе нет role")
	}
	return st, nil
}

// IsLeader реализует role.PatroniChecker.
func (c *Client) IsLeader(ctx context.Context) (bool, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return false, err
	}
	return st.IsLeader(), nil
}

// DynamicConfig - таймауты из GET /config (общая динамическая конфигурация в DCS).
type DynamicConfig struct {
	TTL          int `json:"ttl"`
	LoopWait     int `json:"loop_wait"`
	RetryTimeout int `json:"retry_timeout"`
}

// Config - GET /config.
func (c *Client) Config(ctx context.Context) (DynamicConfig, error) {
	var dc DynamicConfig
	if err := c.getJSON(ctx, "/config", &dc); err != nil {
		return DynamicConfig{}, err
	}
	return dc, nil
}

// CheckTimings проверяет инварианты fencing и возвращает список нарушений:
//   - loop_wait + 2*retry_timeout <= ttl: лидер успевает демоутиться до истечения ключа;
//   - fenceDeadline < ttl: telemux прекращает запись раньше, чем реплика вправе промоутиться.
//
// fenceDeadline = fence_after + период проверки + таймаут пробы.
func (dc DynamicConfig) CheckTimings(fenceDeadline time.Duration) []string {
	var out []string
	if dc.TTL <= 0 {
		return []string{"patroni: ttl не задан в динамической конфигурации"}
	}
	if dc.LoopWait+2*dc.RetryTimeout > dc.TTL {
		out = append(out, fmt.Sprintf(
			"patroni: loop_wait(%d) + 2*retry_timeout(%d) > ttl(%d) - лидер может не успеть демоутиться",
			dc.LoopWait, dc.RetryTimeout, dc.TTL))
	}
	ttl := time.Duration(dc.TTL) * time.Second
	if fenceDeadline >= ttl {
		out = append(out, fmt.Sprintf(
			"telemux: fence_after + интервал + таймаут = %s >= ttl Patroni %s - fenced наступит позже возможного промоута",
			fenceDeadline, ttl))
	}
	return out
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("patroni %s: %w", path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("patroni %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("patroni %s: чтение ответа: %w", path, err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("patroni %s: код %d, не JSON: %w", path, resp.StatusCode, err)
	}
	return nil
}
