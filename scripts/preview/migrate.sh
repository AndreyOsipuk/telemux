#!/bin/bash
# Применяет к telemux_preview миграции новее прод-базы (номер в /opt/telemux-preview/base).
# Новые миграции идемпотентны (IF NOT EXISTS), повторный прогон безопасен.
set -euo pipefail
D=/opt/telemux-preview
base=$(cat "$D/base")
cd /tmp
for m in "$D"/migrations/*.sql; do
  n=$(basename "$m"); n=${n%%_*}
  if [ "$((10#$n))" -gt "$((10#$base))" ]; then
    sudo -u postgres psql -q -v ON_ERROR_STOP=1 -d telemux_preview -c "SET client_min_messages=warning" -c "SET ROLE telemux" -f - < "$m" >/dev/null
    echo "миграция $(basename "$m") применена"
  fi
done
