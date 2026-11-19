-- telemux: одноразовые токены установки агента («добавить машину одной командой»).
-- Мастер выдаёт команду curl ... | sudo bash; скрипт меняет этот токен на токен
-- агента (agent_tokens). Хранится только sha256 токена. Живёт 1 час, тратится
-- один раз (used_at). Не путать с join_tokens - это вход узлов управления.
-- Идемпотентна (IF NOT EXISTS).

BEGIN;

CREATE TABLE IF NOT EXISTS agent_join_tokens (
    token_sha256 TEXT PRIMARY KEY,
    target       TEXT NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Не больше одного неиспользованного токена на машину: новый выпуск удаляет
-- прежние неиспользованные, индекс страхует от гонки двух выпусков.
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_join_tokens_live
    ON agent_join_tokens (target) WHERE used_at IS NULL;

COMMIT;
