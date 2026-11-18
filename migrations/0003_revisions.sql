-- telemux: ревизии желаемого состояния машин (ADR 0001, этап 1 плана).
-- Мастер пишет desired_config, агенты на машинах отчитываются в observed_state.
-- Идемпотентна (IF NOT EXISTS).

BEGIN;

-- Старый секрет после смены ключа: действует до previous_secret_expires_at,
-- в конфиг telemt пишется отдельной записью <username>_legacy.
ALTER TABLE users ADD COLUMN IF NOT EXISTS previous_secret TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS previous_secret_expires_at TIMESTAMPTZ;

-- Машины флота.
CREATE TABLE IF NOT EXISTS targets (
    code        TEXT PRIMARY KEY,                    -- ps1..ps8, pr1..pr4
    kind        TEXT NOT NULL CHECK (kind IN ('exit', 'relay')),
    relay_code  TEXT,                                -- для выхода: релей, к которому он держит туннель
    resources   TEXT[] NOT NULL DEFAULT '{}',        -- какие ресурсы машине положены (telemt.users, ...)
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Глобальная монотонная ревизия: одна публикация - один номер на все машины.
CREATE SEQUENCE IF NOT EXISTS config_revision_seq;

-- Желаемое состояние: строка на (машина, ресурс, ревизия).
CREATE TABLE IF NOT EXISTS desired_config (
    target      TEXT   NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    resource    TEXT   NOT NULL,
    revision    BIGINT NOT NULL,
    sha256      TEXT   NOT NULL,
    content     BYTEA,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (target, resource, revision)
);
CREATE INDEX IF NOT EXISTS idx_desired_latest ON desired_config (target, resource, revision DESC);

-- Что машина реально применила (одна строка на машину и ресурс).
CREATE TABLE IF NOT EXISTS observed_state (
    target        TEXT   NOT NULL,
    resource      TEXT   NOT NULL,
    revision      BIGINT,
    sha256        TEXT,
    status        TEXT   NOT NULL CHECK (status IN ('applied', 'failed', 'pending', 'compared')),
    error         TEXT,
    agent_version TEXT,
    via           TEXT,                              -- через какой мост пришёл отчёт
    seen_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (target, resource)
);

-- Журнал применений (для панели и разбора инцидентов).
CREATE TABLE IF NOT EXISTS apply_log (
    id        BIGSERIAL PRIMARY KEY,
    target    TEXT   NOT NULL,
    resource  TEXT   NOT NULL,
    revision  BIGINT,
    status    TEXT   NOT NULL,
    error     TEXT,
    at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_apply_log_target_at ON apply_log (target, at DESC);

COMMIT;
