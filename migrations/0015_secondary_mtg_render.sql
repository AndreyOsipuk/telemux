-- Второй процесс mtg на одном релее получает собственную строку сравнения.
BEGIN;

ALTER TABLE server_render DROP CONSTRAINT IF EXISTS server_render_resource_check;
ALTER TABLE server_render ADD CONSTRAINT server_render_resource_check
    CHECK (resource IN ('telemt.config', 'mtg.config', 'mtg.secondary.config'));

COMMIT;
