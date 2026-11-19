#!/bin/bash
# Локальная разработка панели на живых данных превью: SSH-туннель к превью (сам
# переподключается при обрыве) и dev-сервер Vite с горячей перезагрузкой. Правки в
# internal/server/web/src видны в браузере сразу, сборка и выкладка не нужны.
#
#   PREVIEW_SSH=<ssh-алиас машины с превью> scripts/preview/panel-dev.sh
#
# Панель - http://localhost:5173, вход - логин и пароль превью. Превью - отдельная база
# и юнит без связи с флотом: правки серверов и машин из локальной панели прод не трогают.
set -euo pipefail
: "${PREVIEW_SSH:?задайте PREVIEW_SSH - ssh-алиас машины с превью}"
LOCAL_PORT=${PREVIEW_LOCAL_PORT:-18091}
REMOTE=${PREVIEW_REMOTE:-127.0.0.1:8091}
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)/internal/server/web"
[ -d node_modules ] || npm ci
(
  while true; do
    ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 \
      -L "$LOCAL_PORT:$REMOTE" "$PREVIEW_SSH" || true
    sleep 2
  done
) &
TUNNEL=$!
trap 'kill $TUNNEL 2>/dev/null; pkill -f "$LOCAL_PORT:$REMOTE" 2>/dev/null || true' EXIT INT TERM
TELEMUX_DEV_BACKEND="http://127.0.0.1:$LOCAL_PORT" npx vite --port "${PANEL_PORT:-5173}" --strictPort
