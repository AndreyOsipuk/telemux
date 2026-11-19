-- telemux: машины как ноды 3x-ui (ADR 0007, этап C) - история нагрузки и тревоги.
-- machine_metrics - строка на машину и минуту из runtime-отчёта (хранение 8 дней),
-- machine_events - лента тревог для бота (хранение 30 дней), состояние детектора
-- тревог - последний известный статус машины и последняя ревизия ресурса, о провале
-- которой уже сообщили. Идемпотентна (IF NOT EXISTS).

BEGIN;

-- Нагрузка машины по минутам. Повтор отчёта в ту же минуту перезаписывает строку.
-- NULL в нагрузке - агент не прислал host.
CREATE TABLE IF NOT EXISTS machine_metrics (
    code         TEXT NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    at           TIMESTAMPTZ NOT NULL,                -- начало минуты
    cpu          REAL,                                -- %
    mem          REAL,                                -- %
    disk         REAL,                                -- %
    load1        REAL,
    rx_bps       DOUBLE PRECISION,                    -- бит/с
    tx_bps       DOUBLE PRECISION,                    -- бит/с
    connections  BIGINT  NOT NULL DEFAULT 0,
    users_online INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (code, at)
);
CREATE INDEX IF NOT EXISTS idx_machine_metrics_at ON machine_metrics (at);

-- Лента тревог. Без внешнего ключа на targets: событие удалённой машины должно
-- дойти до бота, а журнал - пережить машину.
CREATE TABLE IF NOT EXISTS machine_events (
    id      BIGSERIAL PRIMARY KEY,
    at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    machine TEXT NOT NULL,
    kind    TEXT NOT NULL CHECK (kind IN ('down', 'up', 'apply_failed')),
    detail  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_machine_events_at ON machine_events (at);

-- Состояние детектора: молчит ли машина (по последнему проходу). Строки нет -
-- машину ещё не видели или её выключили (при выключении строка удаляется).
CREATE TABLE IF NOT EXISTS machine_alert_state (
    code       TEXT PRIMARY KEY REFERENCES targets(code) ON DELETE CASCADE,
    down       BOOLEAN NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Последняя ревизия ресурса машины, о провале применения которой уже есть событие:
-- одно событие apply_failed на ревизию, даже если агент повторяет отчёт.
CREATE TABLE IF NOT EXISTS machine_apply_alerts (
    code     TEXT NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    resource TEXT NOT NULL,
    revision BIGINT NOT NULL,
    at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (code, resource)
);

COMMIT;
