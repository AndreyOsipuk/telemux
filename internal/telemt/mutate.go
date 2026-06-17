package telemt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/AndreyOsipuk/telemux/internal/telemtsync"
)

// APIError — типизированная ошибка мутации (для распознавания 409/403/404).
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("telemt API HTTP %d: %s", e.Status, e.Body) }

// RevisionConflict — кто-то изменил конфиг параллельно (If-Match не совпал) →
// нужно перечитать список и пересчитать diff.
func (e *APIError) RevisionConflict() bool {
	return e.Status == http.StatusConflict && strings.Contains(e.Body, "revision")
}

// ReadOnly — нода в read_only=true, мутации запрещены.
func (e *APIError) ReadOnly() bool {
	return e.Status == http.StatusForbidden && strings.Contains(e.Body, "read_only")
}

// AlreadyDone — create существующего / delete отсутствующего: трактуем как успех (идемпотентность).
func (e *APIError) AlreadyDone() bool {
	if e.Status == http.StatusNotFound {
		return true // delete несуществующего
	}
	return e.Status == http.StatusConflict && strings.Contains(e.Body, "user_exists")
}

// mutate выполняет POST/PATCH/DELETE с телом и If-Match; возвращает новый revision.
func (c *Client) mutate(ctx context.Context, method, path string, body any, ifMatch string) (string, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return "", err
	}
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("%s %s: разбор envelope: %w", method, path, err)
	}
	return env.Revision, nil
}

type createReq struct {
	Username          string  `json:"username"`
	Secret            string  `json:"secret,omitempty"`
	MaxTCPConns       *int    `json:"max_tcp_conns,omitempty"`
	ExpirationRFC3339 *string `json:"expiration_rfc3339,omitempty"`
	Enabled           *bool   `json:"enabled,omitempty"`
}

// ApplyOp применяет одну операцию синхронизации, возвращает новый revision ноды.
// ifMatch — текущий revision (для CAS); "" чтобы не проверять.
func (c *Client) ApplyOp(ctx context.Context, op telemtsync.SyncOp, ifMatch string) (string, error) {
	switch op.Kind {
	case telemtsync.OpCreate:
		enabled := true
		return c.mutate(ctx, http.MethodPost, "/v1/users", createReq{
			Username:          op.Username,
			Secret:            op.Secret,
			MaxTCPConns:       op.MaxTCPConns,
			ExpirationRFC3339: op.ExpirationRFC3339,
			Enabled:           &enabled,
		}, ifMatch)

	case telemtsync.OpPatch:
		// JSON Merge Patch: только изменённые поля; nil-указатель → JSON null (Remove).
		body := map[string]any{}
		if op.Fields.SetExpiration {
			body["expiration_rfc3339"] = op.Fields.ExpirationRFC3339
		}
		if op.Fields.SetMaxTCPConns {
			body["max_tcp_conns"] = op.Fields.MaxTCPConns
		}
		if op.Fields.Enabled != nil {
			body["enabled"] = *op.Fields.Enabled
		}
		return c.mutate(ctx, http.MethodPatch, "/v1/users/"+op.Username, body, ifMatch)

	case telemtsync.OpDelete:
		return c.mutate(ctx, http.MethodDelete, "/v1/users/"+op.Username, nil, ifMatch)

	default:
		return "", fmt.Errorf("неизвестная операция: %q", op.Kind)
	}
}
