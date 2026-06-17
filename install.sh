#!/usr/bin/env bash
# telemux — установка «на железо» (бинарь + systemd). Идемпотентно.
#   curl -fsSL https://raw.githubusercontent.com/AndreyOsipuk/telemux/main/install.sh | sudo bash
set -euo pipefail

OWNER="AndreyOsipuk"
REPO="telemux"
BIN_DEST="/usr/local/bin/telemux"
CONF_DIR="/etc/telemux"
UNIT="/etc/systemd/system/telemux.service"

[ "$(id -u)" -eq 0 ] || { echo "нужен root (sudo)"; exit 1; }
for c in curl sha256sum systemctl; do command -v "$c" >/dev/null || { echo "нет $c"; exit 1; }; done

# Архитектура → имя артефакта goreleaser.
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "неизвестная архитектура $(uname -m)"; exit 1 ;;
esac
ASSET="telemux_linux_${ARCH}"

echo "==> последняя версия с GitHub"
TAG=$(curl -fsSL "https://api.github.com/repos/${OWNER}/${REPO}/releases/latest" | grep -oE '"tag_name":\s*"[^"]+"' | head -1 | grep -oE '[^"]+$')
[ -n "${TAG:-}" ] || { echo "не удалось определить релиз"; exit 1; }
echo "    версия: ${TAG}"

BASE="https://github.com/${OWNER}/${REPO}/releases/download/${TAG}"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

echo "==> скачивание ${ASSET} + checksums.txt"
curl -fsSL "${BASE}/${ASSET}" -o "${TMP}/telemux"
curl -fsSL "${BASE}/checksums.txt" -o "${TMP}/checksums.txt"

echo "==> проверка SHA256"
WANT=$(grep " ${ASSET}\$" "${TMP}/checksums.txt" | awk '{print $1}')
GOT=$(sha256sum "${TMP}/telemux" | awk '{print $1}')
[ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || { echo "checksum не совпал (ждали $WANT, получили $GOT)"; exit 1; }

echo "==> установка бинаря → ${BIN_DEST}"
install -m0755 "${TMP}/telemux" "${BIN_DEST}"

echo "==> конфиг ${CONF_DIR}/telemux.env"
mkdir -p "${CONF_DIR}"
if [ ! -f "${CONF_DIR}/telemux.env" ]; then
  curl -fsSL "https://raw.githubusercontent.com/${OWNER}/${REPO}/${TAG}/deploy/telemux.env.example" -o "${CONF_DIR}/telemux.env" 2>/dev/null || \
    cat > "${CONF_DIR}/telemux.env" <<'EOF'
DATABASE_URL=postgres://telemux:CHANGE_ME@127.0.0.1:5432/telemux?sslmode=disable
TELEMT_API_URL=http://127.0.0.1:9091
TELEMT_API_AUTH=
TELEMUX_LISTEN=127.0.0.1:8080
EOF
  echo "    создан ${CONF_DIR}/telemux.env — ОТРЕДАКТИРУЙТЕ DATABASE_URL перед стартом"
else
  echo "    ${CONF_DIR}/telemux.env уже есть — не трогаю"
fi

echo "==> systemd-юнит ${UNIT}"
curl -fsSL "https://raw.githubusercontent.com/${OWNER}/${REPO}/${TAG}/deploy/telemux.service" -o "${UNIT}" 2>/dev/null || \
  cat > "${UNIT}" <<'EOF'
[Unit]
Description=telemux — панель управления кластером telemt
After=network-online.target postgresql.service
Wants=network-online.target
[Service]
Type=simple
EnvironmentFile=/etc/telemux/telemux.env
ExecStart=/usr/local/bin/telemux serve
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable telemux >/dev/null 2>&1 || true

echo
echo "✅ telemux ${TAG} установлен ($(telemux --version 2>/dev/null || echo '?'))."
echo "   1) отредактируйте ${CONF_DIR}/telemux.env (DATABASE_URL)"
echo "   2) накатите схему:  psql \"\$DATABASE_URL\" -f migrations/0001_init.sql"
echo "   3) запустите:       systemctl start telemux"
echo "   4) обновление:      telemux update"
