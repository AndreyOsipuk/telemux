-- telemux: серверы - описание того, что видит клиент (ADR 0005, этап B1).
-- «Сервер N»: страна, движок, релей и машина за рубежом, лимиты, настройки движка
-- и точки входа (одна строка server_endpoints = одна ссылка). Пока справочник:
-- ни на что не влияет. Идемпотентна (IF NOT EXISTS).

BEGIN;

CREATE TABLE IF NOT EXISTS servers (
    no                  SMALLINT PRIMARY KEY CHECK (no BETWEEN 1 AND 99),
    name                TEXT NOT NULL CHECK (btrim(name) <> ''),
    emoji               TEXT NOT NULL DEFAULT '',
    country             TEXT NOT NULL DEFAULT '' CHECK (country ~ '^[a-z]{2}$' OR country = ''),
    engine              TEXT NOT NULL CHECK (engine IN ('telemt', 'mtg')),
    -- Машины сервера: релей в РФ (вид relay) и машина за рубежом (вид exit).
    -- Вид проверяет приложение; машину, на которую ссылается сервер, не удалить.
    relay_code          TEXT REFERENCES targets(code) ON DELETE RESTRICT,
    node_code           TEXT REFERENCES targets(code) ON DELETE RESTRICT,
    state               TEXT NOT NULL DEFAULT 'active'
                        CHECK (state IN ('active', 'maintenance', 'test', 'disabled')),
    relay_only          BOOLEAN NOT NULL DEFAULT FALSE,
    node_retired        BOOLEAN NOT NULL DEFAULT FALSE,
    tls_domain          TEXT NOT NULL DEFAULT '',
    extra_tls_domains   TEXT[] NOT NULL DEFAULT '{}',
    user_max_tcp_conns  INTEGER CHECK (user_max_tcp_conns > 0),   -- NULL - общий по умолчанию
    user_max_unique_ips INTEGER CHECK (user_max_unique_ips > 0),  -- NULL - без лимита
    settings            JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(settings) = 'object'),
    note                TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_servers_relay ON servers (relay_code);
CREATE INDEX IF NOT EXISTS idx_servers_node ON servers (node_code);

CREATE TABLE IF NOT EXISTS server_endpoints (
    id         BIGSERIAL PRIMARY KEY,
    server_no  SMALLINT NOT NULL REFERENCES servers(no) ON DELETE CASCADE,
    protocol   TEXT NOT NULL CHECK (protocol IN ('ee', 'dd', 'web')),
    host       TEXT NOT NULL CHECK (host <> ''),
    port       INTEGER NOT NULL DEFAULT 443 CHECK (port BETWEEN 1 AND 65535), -- для web не используется
    tls_domain TEXT NOT NULL DEFAULT '',                                      -- домен в ee-секрете
    via_relay  BOOLEAN NOT NULL DEFAULT TRUE,
    label      TEXT NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 100,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    note       TEXT NOT NULL DEFAULT '',
    CHECK (protocol <> 'ee' OR tls_domain <> '')
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_server_endpoints_link
    ON server_endpoints (server_no, protocol, host, port);

COMMIT;
