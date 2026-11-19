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
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
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

// codedError - ошибка ввода со своим машинным кодом вместо кода по статусу.
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// codedErr оборачивает ошибку ввода машинным кодом (например log_period_invalid).
func codedErr(code string, err error) error { return &codedError{code: code, err: err} }

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
	code := errCodeFor(status)
	var ce *codedError
	if status < 500 && errors.As(err, &ce) {
		code = ce.code
	}
	writeJSONStatus(w, status, errorBody{Code: code, Message: msg, Error: msg})
}

// writeStoreErr классифицирует ошибку хранилища: недоступность базы - 503,
// данные запроса, которые база отвергла, - 4xx, остальное - 500 (баг).
// Для 4xx клиенту - обезличенный текст: в ошибке базы SQL, имена таблиц и
// значения ключей; реальная ошибка - только в лог (Warn: это не сбой сервера).
func (s *Server) writeStoreErr(w http.ResponseWriter, err error) {
	status := storeErrStatus(err)
	if status >= 500 {
		writeErr(w, status, err, s.deps.Log)
		return
	}
	s.deps.Log.Warn("http: база отвергла запрос", "code", status, "err", err)
	msg := storeClientMsg(err)
	writeJSONStatus(w, status, errorBody{Code: errCodeFor(status), Message: msg, Error: msg})
}

// storeClientMsg - текст клиенту для 4xx, вызванного ошибкой базы (без деталей базы).
func storeClientMsg(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch code := pgErr.Code; {
		case code == "25006":
			return "узел принимает только чтение: повторите запрос на мастере"
		case code == "23505":
			return "такая запись уже есть"
		case code == "40P01", code == "40001":
			return "конфликт параллельных изменений: повторите запрос"
		case code == "23503":
			return "запрос ссылается на несуществующую запись"
		}
	}
	return "данные запроса отвергнуты базой: проверьте значения"
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
