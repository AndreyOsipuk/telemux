package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Ответ с ошибкой: {code, message}. Поле error - копия message для старой
// панели, которая читает только его.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// errCodeFor - машинный код ошибки по HTTP-статусу.
func errCodeFor(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusGone:
		return "gone"
	case http.StatusUnprocessableEntity:
		return "unprocessable"
	case http.StatusNotImplemented:
		return "not_implemented"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		if status >= 500 {
			return "internal"
		}
		return "error"
	}
}

// writeErr пишет ошибку в формате {code, message}. Реальная ошибка всегда
// уходит в лог. Для 4xx клиенту отдаётся текст ошибки (это наши сообщения о
// вводе), для 5xx - только общий текст: детали базы и окружения наружу не идут.
func writeErr(w http.ResponseWriter, status int, err error, log *slog.Logger) {
	msg := err.Error()
	switch {
	case status == http.StatusServiceUnavailable:
		log.Error("http: сервис недоступен", "code", status, "err", err)
		msg = "сервис временно недоступен, повторите позже"
	case status >= 500:
		log.Error("http: внутренняя ошибка", "code", status, "err", err)
		msg = "внутренняя ошибка"
	default:
		log.Warn("http: ошибка запроса", "code", status, "err", err)
	}
	writeJSONStatus(w, status, errorBody{Code: errCodeFor(status), Message: msg, Error: msg})
}

// writeStoreErr классифицирует ошибку хранилища: недоступность базы - 503,
// данные запроса, которые база отвергла, - 4xx, остальное - 500 (баг).
func (s *Server) writeStoreErr(w http.ResponseWriter, err error) {
	writeErr(w, storeErrStatus(err), err, s.deps.Log)
}

func storeErrStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return http.StatusServiceUnavailable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return http.StatusServiceUnavailable
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return http.StatusServiceUnavailable
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		switch {
		case strings.HasPrefix(code, "08"), code == "57014", code == "57P01", code == "53300":
			return http.StatusServiceUnavailable // связь, таймаут, рестарт, нет соединений
		case code == "25006":
			return http.StatusConflict // запись в read-only транзакции: узел не мастер
		case code == "23505", code == "40P01", code == "40001":
			return http.StatusConflict // дубль, дедлок, сериализация
		case code == "23503", code == "23502", code == "23514", code == "P0001", strings.HasPrefix(code, "22"):
			return http.StatusBadRequest // данные запроса отвергнуты базой
		}
	}
	return http.StatusInternalServerError
}

// writeJSONStatus пишет JSON с нужным статусом: заголовок ставится до WriteHeader.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
