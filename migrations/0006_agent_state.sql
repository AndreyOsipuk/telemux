-- Последний отчёт агента о машине целиком (одна строка на машину): жив ли
-- агент, версия, через какой мост, результат локальных проверок (мост на
-- машине-выходе и т.п.). Есть и у машин без ресурсов - выходов без telemt и mtg.
CREATE TABLE IF NOT EXISTS agent_state (
    target        TEXT PRIMARY KEY,
    agent_version TEXT,
    via           TEXT,
    checks        JSONB NOT NULL DEFAULT '[]',
    seen_at       TIMESTAMPTZ NOT NULL
);
