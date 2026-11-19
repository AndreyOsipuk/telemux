#!/bin/bash
# Выкладка превью: сборка панели и бинаря из рабочей копии, копирование на машину
# превью, новые миграции, рестарт юнита. Прод (юнит telemux, его база) не трогается.
#
#   PREVIEW_SSH=<ssh-алиас> scripts/preview/deploy.sh [--no-web]   (--no-web - без сборки панели)
set -euo pipefail
: "${PREVIEW_SSH:?задайте PREVIEW_SSH - ssh-алиас машины с превью}"
ROOT=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
D=/opt/telemux-preview
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
cd "$ROOT"
if [ "${1:-}" != "--no-web" ]; then (cd internal/server/web && npm run build >/dev/null); fi
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=preview-$(git rev-parse --short HEAD)" -o "$OUT/telemux" ./cmd/telemux
tar -C "$ROOT" -czf "$OUT/mig.tgz" migrations
tar -C "$ROOT/scripts/preview" -czf "$OUT/scripts.tgz" migrate.sh snapshot.sh live-sync.sh
scp -q "$OUT/telemux" "$PREVIEW_SSH:$D/telemux.new"
scp -q "$OUT/mig.tgz" "$OUT/scripts.tgz" "$PREVIEW_SSH:$D/"
ssh "$PREVIEW_SSH" "set -e; cd $D; mv telemux.new telemux; chmod 755 telemux
  rm -rf migrations; tar --no-same-owner -xzf mig.tgz; tar --no-same-owner -xzf scripts.tgz; chmod 700 migrate.sh snapshot.sh live-sync.sh; rm mig.tgz scripts.tgz
  ./migrate.sh
  systemctl restart telemux-preview; sleep 2; systemctl is-active telemux-preview
  curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:8091/"
