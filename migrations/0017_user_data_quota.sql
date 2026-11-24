-- Страховочная квота telemt на один ключ. Ноль означает отсутствие лимита.
BEGIN;

ALTER TABLE users ADD COLUMN IF NOT EXISTS data_quota_bytes BIGINT NOT NULL DEFAULT 0
    CHECK (data_quota_bytes >= 0);

COMMIT;
