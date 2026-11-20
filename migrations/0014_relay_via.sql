-- telemux: релей - через свою зарубежную ноду (ADR 0009, спецификация relay-via-exit).
-- agent_state.relay_via / relay_via_at - через мост какого выхода пришёл последний
-- отчёт агента (заголовок X-Telemux-Bridge); agent_runtime.bridge - сводка моста в
-- runtime-отчёте выхода; pair_alert_state - состояние детектора тревог по парам
-- (релей, выход); machine_events - новые виды событий и выход у события пары.
-- Идемпотентна: повторный накат ничего не ломает. Старый код новые колонки не читает.

BEGIN;

ALTER TABLE agent_state ADD COLUMN IF NOT EXISTS relay_via    TEXT;
ALTER TABLE agent_state ADD COLUMN IF NOT EXISTS relay_via_at TIMESTAMPTZ;

ALTER TABLE agent_runtime ADD COLUMN IF NOT EXISTS bridge JSONB;

-- Состояние детектора по паре (релей, выход): последний статус, о котором пара что-то
-- утверждает (ok, relay_silent). exit_down строку не меняет: о релее пара тогда молчит.
-- Особые строки: (релей, чужой выход, foreign_bridge) - релей ходит через чужой мост;
-- (релей, '', no_exit) - у релея нет своих нод. Строки нет - пару ещё не видели.
CREATE TABLE IF NOT EXISTS pair_alert_state (
    relay      TEXT NOT NULL REFERENCES targets(code) ON DELETE CASCADE,
    exit       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL CHECK (status IN ('ok', 'relay_silent', 'foreign_bridge', 'no_exit')),
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (relay, exit)
);

-- Выход у события пары (relay_silent, relay_ok, foreign_bridge); у остальных - NULL.
ALTER TABLE machine_events ADD COLUMN IF NOT EXISTS exit TEXT;

ALTER TABLE machine_events DROP CONSTRAINT IF EXISTS machine_events_kind_check;
ALTER TABLE machine_events ADD CONSTRAINT machine_events_kind_check
    CHECK (kind IN ('down', 'up', 'apply_failed', 'relay_silent', 'relay_ok', 'foreign_bridge', 'relay_no_exit'));

COMMIT;
