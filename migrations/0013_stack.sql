-- telemux: обновление telemt и mtg-multi кнопкой (ADR 0008, ресурс stack).
-- stack_releases - последний релиз GitHub по продукту (мастер проверяет раз в 6 часов и
-- по кнопке), stack_updates - последнее обновление продукта на машине через telemux.
-- Идемпотентна (IF NOT EXISTS).

BEGIN;

-- Последний релиз продукта. Ошибка проверки не стирает прошлый результат: version, tag
-- и published_at остаются, error - текст последней ошибки (NULL - проверка удалась).
-- version NULL - релиз ещё ни разу не узнали.
CREATE TABLE IF NOT EXISTS stack_releases (
    product      TEXT PRIMARY KEY CHECK (product IN ('telemt', 'mtg')),
    repo         TEXT NOT NULL,
    version      TEXT,
    tag          TEXT,
    published_at TIMESTAMPTZ,
    checked_at   TIMESTAMPTZ,
    error        TEXT
);

-- Последнее обновление продукта на машине. revision - ревизия ресурса stack, в которой
-- машине опубликован новый бинарь. status pending меняется на applied или failed по
-- отчёту агента (observed_state ресурса stack достиг revision) или по таймауту.
CREATE TABLE IF NOT EXISTS stack_updates (
    target       TEXT NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    product      TEXT NOT NULL CHECK (product IN ('telemt', 'mtg')),
    from_version TEXT,
    to_version   TEXT NOT NULL,
    revision     BIGINT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'applied', 'failed')),
    error        TEXT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ,
    PRIMARY KEY (target, product)
);
CREATE INDEX IF NOT EXISTS idx_stack_updates_pending ON stack_updates (target) WHERE status = 'pending';

COMMIT;
