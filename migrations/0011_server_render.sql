-- telemux: конфиги telemt и mtg собирает telemux (ADR 0006, этап B2).
-- servers.config_source - кто публикует telemt.config / mtg.config машин сервера:
-- бот (bot, по умолчанию) или сам telemux (telemux). server_render - последнее
-- сравнение собранного кандидата с текущей ревизией ресурса машины.
-- Идемпотентна (IF NOT EXISTS).

BEGIN;

ALTER TABLE servers ADD COLUMN IF NOT EXISTS config_source TEXT NOT NULL DEFAULT 'bot';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'servers_config_source_check' AND conrelid = 'servers'::regclass) THEN
        ALTER TABLE servers ADD CONSTRAINT servers_config_source_check
            CHECK (config_source IN ('bot', 'telemux'));
    END IF;
END $$;

-- Одна строка - ресурс одной машины сервера. Строки, которых больше нет в плане
-- сборки (машину убрали из сервера), цикл удаляет.
CREATE TABLE IF NOT EXISTS server_render (
    server_no     SMALLINT NOT NULL REFERENCES servers(no) ON DELETE CASCADE,
    target        TEXT NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    resource      TEXT NOT NULL CHECK (resource IN ('telemt.config', 'mtg.config')),
    candidate_sha TEXT,                 -- NULL - кандидат не собран (error)
    candidate     TEXT,
    current_sha   TEXT,                 -- NULL - ревизий ресурса у машины ещё нет
    equal         BOOLEAN NOT NULL DEFAULT FALSE,
    error         TEXT NOT NULL DEFAULT '',  -- код: текст; пусто - собран
    rendered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (server_no, target, resource)
);

CREATE INDEX IF NOT EXISTS idx_server_render_target ON server_render (target, resource);

COMMIT;
