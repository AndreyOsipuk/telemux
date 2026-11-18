-- telemux: токены агентов на машинах (этап 2 плана).
-- Хранится только sha256 токена: сам токен выдаётся один раз при выпуске.

BEGIN;

CREATE TABLE IF NOT EXISTS agent_tokens (
    target       TEXT PRIMARY KEY REFERENCES targets(code) ON DELETE CASCADE,
    token_sha256 TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
