# telemux

Панель управления кластером [telemt](https://github.com/telemt/telemt) (MTProxy с FakeTLS).
Один бинарь: ядро синхронизации + (планируется) веб-морда и Telegram-бот. Self-hosted,
открытая альтернатива закрытым панелям. Ставится **и «на железо» (systemd), и в Docker**.

> **Статус:** ранний этап. Готовы ядро синхронизации (`internal/*`) и CLI
> (`probe` / `role` / `sync`). Веб-морда, авто-обновление и кластерный режим — в разработке.

## Зачем

telemt хранит список пользователей на каждой ноде и управляется через machine-API
(`/v1/users`). telemux держит **единый источник истины (PostgreSQL)** и приводит каждую
ноду к нему: считает diff против `/v1/users` и применяет create/patch/delete. Многонодовость
строится на нативной PG-репликации (каждая нода читает локальную реплику) — без брокеров.

## Архитектура (кратко)

- **Источник истины:** PostgreSQL. Запись только на primary (master telemux).
- **Роль ноды = роль её PG** (`pg_is_in_recovery`): primary → master, replica → slave.
  Авто-failover — через [Patroni](https://patroni.readthedocs.io) (etcd-кворум). Split-brain невозможен.
- **Данные на ноды** — PG streaming replication. **Управление** (add-node, статус) — gRPC+mTLS.
- Каждая нода синхронит **свой** telemt из **своей** локальной PG. Наружу API telemt и PG не светятся.

## Требования

- PostgreSQL 14+ (на control-ноде; реплики — на остальных).
- telemt с включённым machine-API (`[server.api]`), доступным локально (`127.0.0.1:9091`).
- Go 1.25+ — только для сборки из исходников.

## Установка

### Вариант A — из исходников (доступно сейчас)

```sh
git clone https://github.com/AndreyOsipuk/telemux && cd telemux
go build -o telemux ./cmd/telemux          # бинарь telemux
# или без Go на хосте — через Docker:
docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 \
  golang:1.25-alpine go build -ldflags "-s -w" -o telemux ./cmd/telemux
```

Применить схему БД:

```sh
psql "$DATABASE_URL" -f migrations/0001_init.sql
```

### Вариант B — бинарь + systemd «на железо» (планируется install-скрипт)

```sh
# (планируется) curl -fsSL https://raw.githubusercontent.com/AndreyOsipuk/telemux/main/install.sh | bash
sudo install -m0755 telemux /usr/local/bin/telemux
sudo tee /etc/systemd/system/telemux.service >/dev/null <<'UNIT'
[Unit]
Description=telemux — telemt cluster panel
After=network-online.target postgresql.service
[Service]
EnvironmentFile=/etc/telemux/telemux.env
ExecStart=/usr/local/bin/telemux serve
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload && sudo systemctl enable --now telemux
```

### Вариант C — Docker / docker-compose (планируется образ)

```yaml
# docker-compose.yml (черновик)
services:
  postgres:
    image: postgres:16
    environment: { POSTGRES_DB: telemux, POSTGRES_PASSWORD: change-me }
    volumes: [ "pgdata:/var/lib/postgresql/data" ]
  telemux:
    image: ghcr.io/andreyosipuk/telemux:latest   # планируется
    environment:
      DATABASE_URL: postgres://postgres:change-me@postgres:5432/telemux
      TELEMT_API_URL: http://host.docker.internal:9091
    depends_on: [ postgres ]
volumes: { pgdata: {} }
```

## Конфигурация

| Параметр | Флаг / env | По умолчанию |
|---|---|---|
| DSN локального PG | `--db` / `DATABASE_URL` | — |
| URL telemt API | `--api` / `TELEMT_API_URL` | `http://127.0.0.1:9091` |
| Authorization для API | `--auth` / `TELEMT_API_AUTH` | пусто (без авторизации) |

PostgreSQL держать на loopback (`listen_addresses='127.0.0.1'`) + фаервол; наружу не открывать.
Реплики стримят с primary по TLS + `pg_hba` allowlist.

## Команды (CLI, доступно сейчас)

```sh
telemux probe --api http://127.0.0.1:9091        # проверить связь с telemt-API ноды
telemux role  --db "$DATABASE_URL"               # роль ноды: master/replica (из pg_is_in_recovery)
telemux sync  --db "$DATABASE_URL" --api http://127.0.0.1:9091          # shadow: показать diff
telemux sync  --db "$DATABASE_URL" --api http://127.0.0.1:9091 --apply  # применить
```

`sync` без `--apply` — **shadow**: считает diff (create/patch/delete), но не меняет ноду.
Есть guard от массового сноса (нужен `--force`, если намеренно).

## Дорожная карта

- [x] Ядро: diff-движок, api-client (`/v1/users` + мутации с `If-Match`), role-detector, sync-loop.
- [ ] Авто-обновление (goreleaser-релизы + `telemux update`: скачать → checksum → swap → restart → авто-откат).
- [ ] Patroni/etcd + install-скрипт + add-node через join-token.
- [ ] gRPC контрол-плейн + rolling-update кластера.
- [ ] Веб-морда (React SPA, вшита в бинарь) + Telegram-бот.

## Лицензия

TBD.
