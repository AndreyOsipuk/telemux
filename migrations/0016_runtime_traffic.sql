BEGIN;

ALTER TABLE agent_runtime ADD COLUMN IF NOT EXISTS mtg_secondary_api_ok boolean NOT NULL DEFAULT false;

-- Последняя подтверждённая пара счётчиков отдельно от снимка подключений.
CREATE TABLE IF NOT EXISTS runtime_traffic_counters (
    target text NOT NULL,
    product text NOT NULL,
    user_key text NOT NULL,
    generation text NOT NULL,
    coverage_since timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    bytes_in bigint NOT NULL CHECK (bytes_in >= 0),
    bytes_out bigint NOT NULL CHECK (bytes_out >= 0),
    PRIMARY KEY (target, product, user_key)
);

-- История сохраняется при удалении машины или пользователя.
CREATE TABLE IF NOT EXISTS runtime_traffic_deltas (
    user_key text NOT NULL,
    observed_at timestamptz NOT NULL,
    target text NOT NULL,
    product text NOT NULL,
    bytes_in bigint NOT NULL CHECK (bytes_in >= 0),
    bytes_out bigint NOT NULL CHECK (bytes_out >= 0),
    PRIMARY KEY (user_key, observed_at, target, product)
);

ALTER TABLE runtime_traffic_counters DROP CONSTRAINT IF EXISTS runtime_traffic_counters_product_check;
ALTER TABLE runtime_traffic_counters ADD CONSTRAINT runtime_traffic_counters_product_check
    CHECK (product IN ('telemt', 'mtg', 'mtg_secondary'));

ALTER TABLE runtime_traffic_deltas DROP CONSTRAINT IF EXISTS runtime_traffic_deltas_product_check;
ALTER TABLE runtime_traffic_deltas ADD CONSTRAINT runtime_traffic_deltas_product_check
    CHECK (product IN ('telemt', 'mtg', 'mtg_secondary'));

CREATE INDEX IF NOT EXISTS runtime_traffic_counters_user_idx
    ON runtime_traffic_counters (user_key, observed_at DESC);

COMMIT;
