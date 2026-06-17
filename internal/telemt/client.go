// Package telemt — HTTP-клиент к machine-API telemt (/v1/*).
//
// Контракт (из исходников telemt, src/api/model.rs):
//   - envelope успеха: {"ok":true,"data":<T>,"revision":"<sha256>"}
//   - GET /v1/health        → data:{status, read_only}
//   - GET /v1/users         → data:[UserInfo...]  (=/v1/stats/users)
//   - авторизация: заголовок `Authorization: <auth_header>` (пустой = без авторизации)
//   - revision поддерживает оптимистичную конкуренцию (If-Match) для мутаций (этап 2)
package telemt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// Client — клиент machine-API одной ноды telemt.
type Client struct {
	BaseURL    string
	AuthHeader string
	HTTP       *http.Client
}

// New создаёт клиент. baseURL вида http://127.0.0.1:9091.
func New(baseURL, authHeader string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		AuthHeader: authHeader,
		HTTP:       &http.Client{Timeout: 10 * time.Second},
	}
}

// envelope — обёртка успешного ответа telemt.
type envelope struct {
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Revision string          `json:"revision"`
}

func (c *Client) get(ctx context.Context, path string) (envelope, error) {
	var env envelope
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return env, err
	}
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return env, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return env, fmt.Errorf("GET %s: чтение тела: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return env, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("GET %s: разбор envelope: %w", path, err)
	}
	if !env.OK {
		return env, fmt.Errorf("GET %s: ok=false", path)
	}
	return env, nil
}

// Health — состояние ноды.
type Health struct {
	Status   string `json:"status"`
	ReadOnly bool   `json:"read_only"`
}

// Health возвращает /v1/health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	env, err := c.get(ctx, "/v1/health")
	if err != nil {
		return Health{}, err
	}
	var h Health
	if err := json.Unmarshal(env.Data, &h); err != nil {
		return Health{}, fmt.Errorf("/v1/health: разбор data: %w", err)
	}
	return h, nil
}

// userInfo — нужные поля из UserInfo (остальное игнорируем).
type userInfo struct {
	Username           string  `json:"username"`
	Enabled            bool    `json:"enabled"`
	MaxTCPConns        *int    `json:"max_tcp_conns"`
	ExpirationRFC3339  *string `json:"expiration_rfc3339"`
	CurrentConnections uint64  `json:"current_connections"`
}

// ListUsers возвращает пользователей ноды (для diff) + revision конфига.
func (c *Client) ListUsers(ctx context.Context) ([]telemtsync.RemoteUser, string, error) {
	env, err := c.get(ctx, "/v1/users")
	if err != nil {
		return nil, "", err
	}
	var raw []userInfo
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, "", fmt.Errorf("/v1/users: разбор data: %w", err)
	}
	out := make([]telemtsync.RemoteUser, len(raw))
	for i, u := range raw {
		out[i] = telemtsync.RemoteUser{
			Username:          u.Username,
			Enabled:           u.Enabled,
			ExpirationRFC3339: u.ExpirationRFC3339,
			MaxTCPConns:       u.MaxTCPConns,
		}
	}
	return out, env.Revision, nil
}
