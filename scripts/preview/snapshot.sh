#!/bin/bash
# Снимок прод-базы telemux в telemux_preview (на машине превью, от root).
# Останавливает превью, пересоздаёт базу, вычищает адреса внешних машин,
# применяет миграции, ставит адреса релеев из базы бота, импортирует серверы
# (servers-import.json - экспорт бота), запускает превью и таймер живых цифр.
set -euo pipefail
D=/opt/telemux-preview
systemctl stop telemux-preview-live.timer telemux-preview 2>/dev/null || true
cd /tmp
sudo -u postgres psql -q -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS telemux_preview" -c "CREATE DATABASE telemux_preview OWNER telemux"
sudo -u postgres pg_dump -Fc --no-owner --no-privileges telemux \
  | sudo -u postgres pg_restore --no-owner --role=telemux -d telemux_preview
# Реестр нод кластера (адреса и API telemt машин) превью не нужен.
sudo -u postgres psql -q -v ON_ERROR_STOP=1 -d telemux_preview -c "DELETE FROM nodes"
"$D/migrate.sh"
# Адреса релеев для сборки telemt.config: pr<N> = public_ip сервера ps<N> из базы бота (только чтение).
sudo -u postgres psql -qAt -d tgbots -c "SELECT format('UPDATE targets SET settings = settings || jsonb_build_object(''address'', %L) WHERE code = %L AND kind = ''relay'';', public_ip, 'pr' || substr(code, 3)) FROM servers WHERE code ~ '^ps[0-9]+$' AND coalesce(public_ip, '') <> ''" \
  | sudo -u postgres psql -q -v ON_ERROR_STOP=1 -d telemux_preview
set -a; . /etc/telemux/preview.env; set +a
# Серверы - из экспорта бота; прежние серверы превью стираются.
if [ -f "$D/servers-import.json" ]; then
  sudo -u postgres psql -q -v ON_ERROR_STOP=1 -d telemux_preview -c "DELETE FROM server_endpoints" -c "DELETE FROM servers"
  "$D/telemux" servers import --db "$DATABASE_URL" "$D/servers-import.json"
fi
systemctl start telemux-preview telemux-preview-live.timer
echo "снимок готов: $(date -u +%FT%TZ)"
