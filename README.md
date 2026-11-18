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
  Авто-failover - через [Patroni](https://patroni.readthedocs.io) (etcd-кворум) плюс второй,
  независимый контур fencing в telemux (см. «Автоматическая смена мастера» ниже).
- **Данные на ноды** — PG streaming replication. **Управление** (add-node, статус) — gRPC+mTLS.
- Каждая нода синхронит **свой** telemt из **своей** локальной PG. Наружу API telemt и PG не светятся.

## Автоматическая смена мастера

Решение и разбор вариантов: [docs/adr/0002-master-failover.md](docs/adr/0002-master-failover.md).
Кратко: три машины управления, на каждой etcd + Patroni + PostgreSQL + telemux.

- **Patroni** решает, где PostgreSQL primary: выборы через кворум etcd, демоут изолированного
  primary до истечения ключа лидера, `pg_rewind` старого primary, синхронная репликация
  `synchronous_mode: quorum` (коммит подтверждён минимум на двух машинах).
- **telemux** решает, кто принимает запись приложений. Узел - мастер, только если одновременно:
  1. локальный PG - primary;
  2. локальный Patroni считает себя лидером (если задан `TELEMUX_PATRONI_URL`);
  3. telemux видит большинство голосующих машин (себя и хотя бы одного соседа из двух) по HTTP
     или по живой streaming-реплике в `pg_stat_replication`;
  4. условия держатся `TELEMUX_MASTER_HOLD` подряд (мастер теряется сразу, получается медленно).

  Без кворума дольше `TELEMUX_FENCE_AFTER` primary становится **fenced**: `/api/leader` отдаёт 503
  (supervisor бота останавливает бота), запись через API - `409 not_master`, а при включённом
  PG-fence PostgreSQL уходит в `default_transaction_read_only` и сессии бота рвутся. Это
  происходит раньше, чем Patroni вправе промоутить реплику (`fence_after + 2*interval < ttl`;
  telemux проверяет инвариант при старте и пишет предупреждение).

Роли, которые видит telemux: `master`, `replica`, `fenced`, `unknown` (локальный PG недоступен).

### Клиенты

| Клиент | Как находит мастера |
|---|---|
| Бот (PG) | libpq multi-host: `postgres://bot@ps1,ps2,ps3/telemux?target_session_attrs=read-write` |
| Бот (лидер процесса) | supervisor опрашивает `GET http://127.0.0.1:8080/api/leader`: 200 - запускать, иначе останавливать |
| Вебхуки платёжек | reverse-proxy на каждой машине, upstream из трёх ботов, переход к следующему на 503 |
| Агенты, мосты, heartbeat | список всех машин; не-мастер отвечает `409 {"error":"not_master","master_url":...}` и `X-Telemux-Master` |

### Эндпоинты

| Метод и путь | Авторизация | Ответ |
|---|---|---|
| `GET /api/leader` | нет (держать за фаерволом) | 200 `{"master":true}` / 503 `{"error":"not_master","master_code","master_url","reason"}` |
| `GET /api/role` | сессия панели | `{"role","is_master","reason","quorum","seen","since","master","master_url"}` |
| `GET /api/cluster/peer` | Bearer cluster-secret | `{"code","role"}` - пинг соседа, канал кворума |
| Любая запись (`/api/users*`, `/api/cluster/users-sync`, `heartbeat`, `join-token`) на не-мастере | как у ручки | 409 `not_master` с подсказкой мастера |

### Развёртывание

Шаблоны: [deploy/etcd/etcd.env.example](deploy/etcd/etcd.env.example),
[deploy/patroni/patroni.yml.example](deploy/patroni/patroni.yml.example), переменные telemux -
в [deploy/telemux.env.example](deploy/telemux.env.example). systemd-режим - пакеты etcd/Patroni и
[deploy/ha/patroni.service](deploy/ha/patroni.service) (служба `postgresql` отключается, PG
запускает Patroni); docker-режим - [deploy/ha/docker-compose.yml](deploy/ha/docker-compose.yml)
на каждой машине (`network_mode: host`). План перевода живого кластера без простоя - в ADR 0002. Права роли telemux в PG: `pg_monitor`;
для PG-fence ещё `pg_signal_backend` и `GRANT ALTER SYSTEM ON PARAMETER
default_transaction_read_only` (PG 15+) или суперпользователь.

Аварийно (telemux не может подняться, а PG остался read-only):

```sh
telemux fence --db "$DATABASE_URL" status
telemux fence --db "$DATABASE_URL" off
```

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

### Вариант B — бинарь + systemd «на железо»

Скрипт `install.sh` качает последний релиз (с проверкой SHA256), ставит бинарь,
создаёт `/etc/telemux/telemux.env` и systemd-юнит (заработает после публикации релиза):

```sh
curl -fsSL https://raw.githubusercontent.com/AndreyOsipuk/telemux/main/install.sh | sudo bash
# затем отредактировать /etc/telemux/telemux.env (DATABASE_URL) и: systemctl start telemux
```

Вручную:

```sh
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
| Authorization для API telemt | `--auth` / `TELEMT_API_AUTH` | пусто (без авторизации) |
| Логин панели | `TELEMUX_ADMIN_USER` | `admin` |
| Пароль панели | `TELEMUX_ADMIN_PASSWORD` | пусто = auth выключен |
| Код этой машины | `TELEMUX_NODE_CODE` | - (обязателен в кластере) |
| Секрет межузловых запросов | `TELEMUX_CLUSTER_SECRET` | - (обязателен при `TELEMUX_PEERS`) |
| Токен бота для `/api/v1/*` | `TELEMUX_BOT_TOKEN` | пусто = только по сессии панели |
| Лимит подключений по умолчанию в `users.toml` | `TELEMUX_DEFAULT_MAX_CONNS` | `50` |
| Соседние голосующие машины | `TELEMUX_PEERS` (`ps2=https://...,ps3=https://...`) | пусто = одиночный режим |
| REST локального Patroni | `TELEMUX_PATRONI_URL` | пусто = не проверять |
| Порог потери кворума до fenced | `TELEMUX_FENCE_AFTER` | `10s` |
| Выдержка до получения мастера | `TELEMUX_MASTER_HOLD` | `5s` |
| Период проверки роли | `TELEMUX_ROLE_INTERVAL` | `2s` |
| Роли приложений для PG-fence | `TELEMUX_PG_FENCE_ROLES` | пусто = PG-fence выключен |

PostgreSQL держать на loopback (`listen_addresses='127.0.0.1'`) + фаервол; наружу не открывать.
Реплики стримят с primary по TLS + `pg_hba` allowlist.

## HTTP API

Контракт - [api/openapi.yaml](api/openapi.yaml), единый источник правды: ответы API
проверяются по нему в тестах, панель генерирует из него типы. Работающий узел отдаёт
спеку на `GET /api/openapi.json`.

API для бота (Bearer `TELEMUX_BOT_TOKEN` или сессия панели, запись - только мастер):

| Метод | Путь | Что делает |
|---|---|---|
| `PUT` | `/api/v1/users` | полный набор пользователей: сверка + публикация ревизии; защита от массового удаления (`409 mass_delete_guard`, обход `?force=true`) |
| `PUT` | `/api/v1/users/{key}` | создать (201) или изменить (200) одного пользователя |
| `DELETE` | `/api/v1/users/{key}` | удалить пользователя |
| `GET` | `/api/v1/revisions/{rev}/status` | на каких машинах ревизия уже применена |

Каждое изменение пользователей публикует **ревизию**: `users.toml` собирается заново
(одинаковые данные - одинаковые байты) и записывается в `desired_config` тем машинам из
`targets`, у которых он отличается. Агенты на машинах забирают ревизии и отчитываются в
`observed_state` (этап 2 плана, [docs/PLAN.md](docs/PLAN.md)).

API агентов на машинах (Bearer-токен машины):

| Метод | Путь | Что делает |
|---|---|---|
| `GET` | `/v1/desired?target=&rev=&wait=` | long-poll: бандл сразу, если есть ревизия новее `rev`, иначе ожидание до `wait` (до 60 с) и 304 |
| `POST` | `/v1/observed` | отчёт агента: ревизия, хеш, статус (`applied`, `failed`, `pending`, `compared`) |

Токен машины даёт доступ только к её бандлу; в базе хранится только хеш токена.

Ошибки - `{code, message}`: 4xx - ошибка запроса, 503 - недоступна база, 500 - баг.

## Агент на машине

Агент сам забирает желаемое состояние у мастера и отчитывается
([ADR 0001](docs/adr/0001-pull-reconcile.md)). Сейчас доступен режим **compare**:
агент сравнивает `users.toml` на диске с желаемым по смыслу (набор пользователей,
секреты, сроки, лимиты; порядок строк и формат не важны) и отправляет отчёт.
Файл он не меняет.

На мастере:

```sh
telemux target --code pr2 --kind relay --resources telemt.users   # машина флота
telemux agent-token --target pr2 > pr2.token                        # токен (прежний перестаёт работать)
telemux publish                                                     # опубликовать ревизию вручную
```

На машине: бинарь `telemux`, токен в `/etc/telemux/agent.token` (chmod 600),
настройки - [deploy/agent.env.example](deploy/agent.env.example) в `/etc/telemux/agent.env`,
юнит - [deploy/telemux-agent.service](deploy/telemux-agent.service).
Результаты сверки видны в `observed_state` и `apply_log`.

## Веб-панель

Вшита в бинарь, открывается на адресе демона (`--listen`, по умолчанию `:8080`). Вход - по
`TELEMUX_ADMIN_USER` / `TELEMUX_ADMIN_PASSWORD`; без пароля панель открыта (только за фаерволом).
Изменения принимает только мастер: на остальных узлах панель работает на просмотр и показывает
адрес мастера.

| Раздел | Что показывает |
|---|---|
| Пары | релей, два его сервера и два выхода: статус, ревизия желаемая и применённая, версия бинаря (с отметкой канарейки), «видели N секунд назад», мост; «применить сейчас», «повторить», история применений |
| Дрейф | только машины, где желаемое не совпадает с применённым дольше порога |
| Пользователи | список с серверной пагинацией, создание, вкл/выкл, продление, удаление |
| Машины | реестр узлов кластера, роль, последний heartbeat |
| Синк | статус последней синхронизации пользователей на ноды и запуск вручную |
| API | swagger-ui по `GET /api/openapi.json` |

«Пары» и «Дрейф» пока на демо-данных: ручек `pairs`, `drift`, `apply-log` на мастере ещё нет.
Светлая и тёмная темы, русский и английский. Разработка и сборка - [internal/server/web/README.md](internal/server/web/README.md).

## Команды (CLI, доступно сейчас)

```sh
telemux probe --api http://127.0.0.1:9091        # проверить связь с telemt-API ноды
telemux role  --db "$DATABASE_URL"               # роль ноды: master/replica (из pg_is_in_recovery)
telemux fence --db "$DATABASE_URL" status        # PG-fence: включён ли read-only (аварийно: off)
telemux sync  --db "$DATABASE_URL" --api http://127.0.0.1:9091          # shadow: показать diff
telemux sync  --db "$DATABASE_URL" --api http://127.0.0.1:9091 --apply  # применить
```

`sync` без `--apply` — **shadow**: считает diff (create/patch/delete), но не меняет ноду.
Есть guard от массового сноса (нужен `--force`, если намеренно).

## Тесты

```sh
go test ./...                                   # unit (без БД)
# интеграционные (нужен PostgreSQL); -p 1 обязателен — пакеты делят одну БД:
TELEMUX_TEST_DSN=postgres://user:pass@127.0.0.1:5432/telemux \
  go test -tags=integration -p 1 ./internal/store ./internal/e2e
```

## Дорожная карта

- [x] Ядро: diff-движок, api-client (`/v1/users` + мутации с `If-Match`), role-detector, sync-loop.
- [ ] Авто-обновление (goreleaser-релизы + `telemux update`: скачать → checksum → swap → restart → авто-откат).
- [x] Автоматическая смена мастера: Patroni/etcd + кворумный fencing в telemux, `/api/leader`, PG-fence.
- [ ] install-скрипт для etcd/Patroni + add-node через join-token.
- [ ] gRPC контрол-плейн + rolling-update кластера.
- [x] Веб-панель (React + antd, вшита в бинарь): каркас, пользователи, машины, синк, API.
- [ ] Панель флота: «Пары», «Дрейф», журнал применений, раскатки - на живых данных мастера.
- [ ] Telegram-бот.

## Лицензия

[GPL-3.0](LICENSE). Часть кода веб-панели основана на [3x-ui](https://github.com/MHSanaei/3x-ui) (GPL-3.0); у заимствованных файлов сохраняется указание на источник.
