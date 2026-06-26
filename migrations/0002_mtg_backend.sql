-- 0002: backend-тип ноды + параметры mtg-multi.
--
-- telemux получает плагинный backend-слой: нода может обслуживаться telemt
-- (machine-API) ИЛИ mtg-multi (config.toml + рестарт по SSH). Тип — в nodes.backend.
-- Существующие ноды по умолчанию telemt (обратная совместимость).

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS backend TEXT NOT NULL DEFAULT 'telemt';

-- SSH-доступ (для backend'ов, доставляющих конфиг по SSH, напр. mtg-multi).
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS ssh_user TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS ssh_port INTEGER;

-- Параметры mtg-multi инстанса (заполнены, если backend='mtg-multi').
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mtg_bind_to         TEXT;  -- 0.0.0.0:8767
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mtg_api_bind_to     TEXT;  -- 127.0.0.1:8081 (/stats)
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mtg_fronting_domain TEXT;  -- static-<code>.proxy-osv.site
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mtg_prefer_ip       TEXT;  -- only-ipv4
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mtg_max_conns       INTEGER; -- throttle; NULL/0 = off
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mtg_config_path     TEXT;  -- /etc/mtg-multi/config.toml
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mtg_reload_cmd      TEXT;  -- systemctl restart mtg-multi
