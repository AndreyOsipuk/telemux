#!/bin/bash
# Живые цифры в превью (таймер раз в минуту): копирует отчёты агентов из прод-базы
# telemux (только чтение) в telemux_preview. Остальные таблицы превью - снимок.
set -uo pipefail
cd /tmp
for t in agent_runtime runtime_user_conns runtime_log_errors agent_state observed_state; do
  sudo -u postgres psql -qAt -d telemux -c "COPY $t TO STDOUT" \
    | sudo -u postgres psql -q -1 -v ON_ERROR_STOP=1 -d telemux_preview -c "TRUNCATE $t" -c "COPY $t FROM STDIN" \
    || echo "live-sync: $t не скопирована" >&2
done

# История нагрузки для графиков. Прод с таблицей machine_metrics - докопируем новые
# строки; старый прод без неё - пишем строку сами из скопированного снимка, как
# RecordMetrics мастера.
if [ "$(sudo -u postgres psql -qAt -d telemux -c "SELECT to_regclass('machine_metrics') IS NOT NULL")" = t ]; then
  since=$(sudo -u postgres psql -qAt -d telemux_preview -c "SELECT coalesce(max(at), now() - interval '8 days') FROM machine_metrics")
  sudo -u postgres psql -qAt -d telemux -c "COPY (SELECT * FROM machine_metrics WHERE at >= '$since') TO STDOUT" \
    | sudo -u postgres psql -q -v ON_ERROR_STOP=1 -d telemux_preview \
        -c "CREATE TEMP TABLE m (LIKE machine_metrics)" -c "COPY m FROM STDIN" \
        -c "INSERT INTO machine_metrics SELECT m.* FROM m JOIN targets t ON t.code = m.code ON CONFLICT (code, at) DO UPDATE SET cpu = EXCLUDED.cpu, mem = EXCLUDED.mem, disk = EXCLUDED.disk, load1 = EXCLUDED.load1, rx_bps = EXCLUDED.rx_bps, tx_bps = EXCLUDED.tx_bps, connections = EXCLUDED.connections, users_online = EXCLUDED.users_online" \
    || echo "live-sync: история нагрузки не скопирована" >&2
  exit 0
fi
h() { echo "CASE WHEN jsonb_typeof(r.host->'$1') = 'number' THEN (r.host->>'$1')::float8 END"; }
sudo -u postgres psql -q -v ON_ERROR_STOP=1 -d telemux_preview -c "
INSERT INTO machine_metrics (code, at, cpu, mem, disk, load1, rx_bps, tx_bps, connections, users_online)
SELECT r.target, date_trunc('minute', r.seen_at), $(h cpu_pct), $(h mem_pct), $(h disk_pct), $(h load1),
       $(h net_rx_mbps) * 1e6, $(h net_tx_mbps) * 1e6, COALESCE(u.conns, 0), COALESCE(u.online, 0)
  FROM agent_runtime r
  LEFT JOIN LATERAL (SELECT sum(c.connections)::bigint AS conns,
                            count(DISTINCT c.user_key) FILTER (WHERE c.connections > 0)::int AS online
                       FROM runtime_user_conns c WHERE c.target = r.target) u ON TRUE
 ON CONFLICT (code, at) DO UPDATE
   SET cpu = EXCLUDED.cpu, mem = EXCLUDED.mem, disk = EXCLUDED.disk, load1 = EXCLUDED.load1,
       rx_bps = EXCLUDED.rx_bps, tx_bps = EXCLUDED.tx_bps,
       connections = EXCLUDED.connections, users_online = EXCLUDED.users_online" \
  || echo "live-sync: история нагрузки не записана" >&2
