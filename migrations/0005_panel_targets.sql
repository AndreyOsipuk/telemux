-- telemux: машины вида panel - MTProto-вход мастер-панели 3x-ui (mtg «через панель»).
-- Настройки входа (inbound_id, node_id, port, node_code) - в targets.settings.

BEGIN;

ALTER TABLE targets DROP CONSTRAINT IF EXISTS targets_kind_check;
ALTER TABLE targets ADD CONSTRAINT targets_kind_check CHECK (kind IN ('exit', 'relay', 'panel'));
ALTER TABLE targets ADD COLUMN IF NOT EXISTS settings JSONB NOT NULL DEFAULT '{}';

COMMIT;
