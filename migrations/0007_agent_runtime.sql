-- Runtime-отчёт агента (раз в минуту): нагрузка машины, версии telemt и mtg,
-- подключения пользователей и счётчики ошибок журнала. Истории нет: хранится
-- только последний снимок машины (IP пользователей - персональные данные), счётчики
-- журнала - 24 часа. Идемпотентна (IF NOT EXISTS).

BEGIN;

-- Последний снимок машины целиком. telemt и mtg - без списка пользователей
-- (он в runtime_user_conns); NULL - на машине нет этого продукта.
CREATE TABLE IF NOT EXISTS agent_runtime (
    target        TEXT PRIMARY KEY REFERENCES targets(code) ON DELETE CASCADE,
    agent_version TEXT,
    host          JSONB,
    telemt        JSONB,
    mtg           JSONB,
    seen_at       TIMESTAMPTZ NOT NULL
);

-- Подключения пользователей по последнему снимку: строки продукта машины
-- заменяются отчётом, в котором API продукта ответил (api_ok=true).
CREATE TABLE IF NOT EXISTS runtime_user_conns (
    target      TEXT   NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    product     TEXT   NOT NULL CHECK (product IN ('telemt', 'mtg')),
    user_key    TEXT   NOT NULL,
    connections BIGINT NOT NULL DEFAULT 0,
    active_ips  TEXT[] NOT NULL DEFAULT '{}',
    bytes_in    BIGINT NOT NULL DEFAULT 0,
    bytes_out   BIGINT NOT NULL DEFAULT 0,
    last_seen   TIMESTAMPTZ,
    seen_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (target, product, user_key)
);

-- Счётчики ошибок журнала telemt за период между отчётами. Чистятся старше 24 ч
-- периодической задачей мастера. Строка уникальна по (машина, начало периода,
-- пользователь, вид): повтор того же отчёта пишется ON CONFLICT DO NOTHING и не
-- удваивает суммы.
CREATE TABLE IF NOT EXISTS runtime_log_errors (
    id           BIGSERIAL PRIMARY KEY,
    target       TEXT   NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    user_key     TEXT   NOT NULL DEFAULT '',
    kind         TEXT   NOT NULL CHECK (kind IN ('limit', 'handshake', 'auth', 'timeout', 'other')),
    count        BIGINT NOT NULL CHECK (count >= 0),
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_runtime_log_errors_period
    ON runtime_log_errors (target, period_start, user_key, kind);
CREATE INDEX IF NOT EXISTS idx_runtime_log_errors_end ON runtime_log_errors (period_end);

COMMIT;
