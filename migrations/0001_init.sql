-- telemux: начальная схема (PostgreSQL).
-- Источник истины для desired-состояния пользователей + реестр нод кластера.
-- Пишется ТОЛЬКО на primary (master telemux); реплики читают (read-only).

BEGIN;

-- Пользователи прокси (desired). В multi-node-модели один и тот же набор
-- активных юзеров раскатывается на каждую ноду (как multi-server у mtproxy).
CREATE TABLE IF NOT EXISTS users (
    id           BIGSERIAL PRIMARY KEY,
    -- Ключ юзера в telemt (имя в [access.users]). Уникальный.
    username     TEXT NOT NULL UNIQUE,
    -- MTProto secret (hex, 32 симв.). Ставится при create; в diff по списку не сверяется.
    secret       TEXT NOT NULL,
    -- Истечение подписки. NULL = без срока.
    expiration_at TIMESTAMPTZ,
    -- Лимит TCP-коннектов. NULL = дефолт ноды.
    max_tcp_conns INTEGER,
    -- Активна ли подписка (desired enabled).
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Частый запрос desired: только активные.
CREATE INDEX IF NOT EXISTS idx_users_enabled ON users (enabled) WHERE enabled;

-- Ноды кластера (для веб-морды + add-node flow + статус роли).
CREATE TABLE IF NOT EXISTS nodes (
    id            BIGSERIAL PRIMARY KEY,
    code          TEXT NOT NULL UNIQUE,          -- ps1, ps2, ...
    name          TEXT NOT NULL DEFAULT '',
    address       TEXT NOT NULL,                 -- хост/IP ноды
    telemt_api_url TEXT NOT NULL DEFAULT 'http://127.0.0.1:9091',
    -- Роль из локального PG ноды (master/replica/unknown). Обновляет агент ноды.
    role          TEXT NOT NULL DEFAULT 'unknown',
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    last_seen_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Одноразовые токены добавления ноды (curl ... | bash join-flow).
CREATE TABLE IF NOT EXISTS join_tokens (
    token       TEXT PRIMARY KEY,               -- случайный, отдаётся один раз
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,                    -- NULL = ещё не использован
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
