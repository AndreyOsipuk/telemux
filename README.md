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
| Период runtime-отчёта агентов (для порога `stale`) | `TELEMUX_RUNTIME_EVERY` | `60s` |
| Через сколько runtime-снимок считается устаревшим (`stale`) | `TELEMUX_RUNTIME_STALE_AFTER` | три периода отчёта (`180s`) |
| Соседние голосующие машины | `TELEMUX_PEERS` (`ps2=https://...,ps3=https://...`) | пусто = одиночный режим |
| REST локального Patroni | `TELEMUX_PATRONI_URL` | пусто = не проверять |
| Порог потери кворума до fenced | `TELEMUX_FENCE_AFTER` | `10s` |
| Выдержка до получения мастера | `TELEMUX_MASTER_HOLD` | `5s` |
| Период проверки роли | `TELEMUX_ROLE_INTERVAL` | `2s` |
| Роли приложений для PG-fence | `TELEMUX_PG_FENCE_ROLES` | пусто = PG-fence выключен |
| Внешний https-адрес API агентов для команды установки | `TELEMUX_AGENT_PUBLIC_URL` (`https://<адрес мастера>:18443`) | пусто = добавление машины отвечает 409 |
| Цепочка iptables для адресов выходов | `TELEMUX_AGENT_FW_CHAIN` | пусто = файрвол не трогается |
| Почта аккаунта ACME (сертификаты WEB) | `TELEMUX_ACME_EMAIL` | пусто = выпуск выключен |
| Файл с токеном Cloudflare для DNS-01 | `TELEMUX_ACME_CF_TOKEN_FILE` | пусто = выпуск выключен |
| Каталог ACME | `TELEMUX_ACME_DIR` | продакшн Let's Encrypt |
| За сколько до конца продлевать сертификат | `TELEMUX_ACME_RENEW_BEFORE` | `720h` (30 дней) |
| Репозиторий релизов telemt (обновление кнопкой) | `TELEMUX_STACK_TELEMT_REPO` | `telemt/telemt` |
| Репозиторий релизов mtg-multi | `TELEMUX_STACK_MTG_REPO` | `dolonet/mtg-multi` |
| Токен GitHub (только от лимита запросов к API) | `TELEMUX_GITHUB_TOKEN` | пусто = без токена |
| Кэш бинарей обновлений на мастере | `TELEMUX_STACK_DIR` | `/var/lib/telemux/stack` |

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
| `GET` | `/api/v1/overview` | обзор для главной страницы панели одним запросом: машины (всего, агент в сети, молчат дольше 10 минут), подключения и уникальные пользователи онлайн, пары с релеем (в порядке / с проблемами), сертификаты (истекают раньше 14 дней, не выпущены; `certs: null` - выпуск выключен) и машины с runtime-полями - не больше 500 (`truncated: true` - флот больше, итоги всё равно по всем) |
| `GET` | `/api/v1/machines?resource=&enabled=&kind=&limit=&offset=` | машины флота: когда отчитывался агент, его версия, мост, проверки, состояние ресурсов (`sha256`, `desired_revision`), из runtime-отчёта - версии telemt и mtg, нагрузка (`host`), `telemt_api_ok`/`mtg_api_ok`, `connections`, `users_online` и `runtime_status` (`online`, `stale` - отчёт старше 180 с, `degraded` - API telemt или mtg не ответил, `offline` - отчёта нет или агент молчит дольше 10 минут); фильтры `resource` (`telemt.users`, `telemt.config`, `mtg.config`, `xui.clients`, `tls.web`, `stack`), `enabled`, `kind` (`exit`, `relay`, `panel`), неизвестное значение - 400; `{data, paging}`, `total` с фильтром, `limit` 1-100 (по умолчанию 20) |
| `GET` | `/api/v1/machines/{code}/runtime` | последний runtime-снимок машины: нагрузка, telemt, mtg (без списков пользователей), `stale` |
| `GET` | `/api/v1/connections?machine=&product=&min_connections=&user=&limit=&offset=` | подключения пользователей по последним снимкам, порядок machine, product, key; `user` - префикс ключа пользователя (`%` и `_` - обычные символы) |
| `GET` | `/api/v1/connections/by-user?user=a&user=b` | для каждого `user` (1-100) - сумма подключений по всем машинам, число машин с подключениями, `online`; один запрос к базе, порядок ответа - как в запросе |
| `GET` | `/api/v1/connections/summary?limit=&offset=` | по машине и продукту: `connections_total`, `users_online`, `stale`, `api_ok`, `error`; строка есть и при `api_ok=false` (данные - последнего удачного снимка или 0) |
| `GET` | `/api/v1/log-errors?since=&machine=&limit=&offset=` | ошибки журнала telemt за окно (`since` обязателен, не раньше 24 ч назад), порядок count по убыванию |
| `GET` | `/api/v1/pairs?limit=&offset=` | пары флота: релей и его выходы, у каждой машины желаемые ревизии и сводный статус; у выхода - `relay_status` (`ok`, `relay_silent`, `exit_down`) и `relay_last_seen`, у пары - `relay_down`, `foreign_bridge` и `status` (`ok`, `down`, `no_exit`), см. «Релей через свою ноду»; машины вне пар - в псевдо-паре `relay: null` в конце |
| `GET` | `/api/v1/drift?threshold=&limit=&offset=` | расхождения применённого с желаемым дольше порога (`threshold` 0-86400 с, по умолчанию 600): `behind`, `failed`, `silent` |
| `GET` | `/api/v1/targets/{code}/apply-log?limit=&offset=` | журнал применений и проверок машины, новые первыми; неизвестная машина - 404 |
| `GET` | `/api/v1/certs?machine=&limit=&offset=` | сертификаты WEB-доменов: строка на (релей, домен), `machine` - только эта машина - `assigned` (назначен `tls.web`), `not_after`, `issued_at`, `last_try_at`, `last_error`; сертификаты и ключи не отдаются |
| `POST` | `/api/v1/targets` | добавить машину (`exit` или `relay`) и сразу получить команду установки агента (201); занятый код - 409 |
| `POST` | `/api/v1/targets/{code}/join` | новая одноразовая команда установки (прежняя сгорает); релей без выходов - 409 |
| `PUT` | `/api/v1/targets/{code}` | включить/выключить машину (`enabled`) и (или) поправить `settings` (слияние, проверка как при добавлении); см. «Машины: включение, проверка связи, нагрузка, тревоги» |
| `DELETE` | `/api/v1/targets/{code}` | удалить выключенную машину (204); включённая - 409 `machine_enabled`, на неё ссылаются серверы, выходы или входы панели - 409 `machine_in_use` с номерами |
| `POST` | `/api/v1/machines/{code}/check` | проверка связи (только мастер): свежесть агента, TCP к точкам входа серверов машины, для `web` - TLS с SNI; 5 с на точку, всего до 20 с |
| `GET` | `/api/v1/machines/{code}/metrics?from=&to=&step=` | история нагрузки (CPU, память, диск, load, сеть, подключения, онлайн), среднее по шагу `1m`/`5m`/`1h`; по умолчанию 24 ч и `5m`; больше 2000 точек - 400 `too_many_points` |
| `GET` | `/api/v1/events?after_id=&limit=` | лента тревог для бота (`down`, `up`, `apply_failed`, по парам - `relay_silent`, `relay_ok`, `foreign_bridge`, `relay_no_exit` с полем `exit`) по возрастанию `id`; `limit` 1-500 (по умолчанию 100) |
| `GET` | `/api/v1/stack?limit=&offset=` | версии telemt и mtg-multi (см. «Обновление telemt и mtg-multi»): последние релизы GitHub (`products`, всегда оба) и релеи с выходами страницей - версия на машине, последняя, `update_available`, `custom_build`, последнее обновление и его статус; на реплике `data` пустой |
| `POST` | `/api/v1/stack/check` | проверить релизы сейчас (только мастер); ошибка GitHub - в `error` продукта, а не 5xx; чаще раза в минуту - 429 `too_many_requests` |
| `POST` | `/api/v1/machines/{code}/update` | обновить продукт на машине (только мастер): `{"product": "telemt"\|"mtg", "version"?, "force"?}`, ответ 202 с ревизией |
| `GET` | `/api/v1/servers?engine=&state=&limit=&offset=` | серверы (см. «Серверы»): описание, точки входа и живые цифры машин сервера (`live.relay`, `live.node`: `seen_at`, `stale`, `api_ok`, `connections`, `users_online` по продукту = движку); фильтры `engine` (`telemt`, `mtg`), `state` |
| `GET` | `/api/v1/servers/{no}` | один сервер; нет - 404 |
| `GET` | `/api/v1/servers/fields?engine=` | каталог полей настроек движка: тип, пределы, значение по умолчанию, группа, способ применения, `danger` |
| `POST` | `/api/v1/servers` | создать сервер (201); номер занят - 409 |
| `PUT` | `/api/v1/servers/{no}` | заменить сервер целиком вместе с точками входа (одна транзакция); смена движка - 400 `engine_immutable` |
| `DELETE` | `/api/v1/servers/{no}` | удалить сервер (204), только `state=disabled`, иначе 409 |
| `PUT` | `/api/v1/servers` | импорт: массив до 100 серверов, upsert по номеру, одна транзакция; `{inserted, updated}` |

Все списки - `limit` 1-100 (по умолчанию 20, больше - 400), `offset` >= 0, ответ
`{data, paging{total, limit, offset}}`; мусор в параметрах - 400. Ручки панели
`pairs`, `drift`, `apply-log`, `certs`, `metrics`, `events` только читают и работают на любом узле, включая реплику.
`stale: true` - снимок старше порога (`TELEMUX_RUNTIME_STALE_AFTER`, по умолчанию 180 с)
или его нет. Если база отвергла данные запроса (4xx), клиенту уходит обезличенный текст,
подробности - в лог мастера.

**Пары.** Страница идёт по релеям (`kind=relay`) по коду; выходы привязаны к релею
через `relay_code`. Выходы без релея (или с несуществующим релеем) и машины `panel`
собраны в псевдо-пару с `relay: null` - она последняя и считается в `total`, если такие
машины есть. Статус машины: `disabled` - выключена; `no_agent` - агент ни разу не
отчитывался; `silent` - отчёт старше 10 минут; `failed` - ресурс в `failed` или проверка
не пройдена; `behind` - ресурс отстаёт от последней желаемой ревизии; иначе `ok`.
Учитываются только ресурсы, назначенные машине в `targets.resources`.

**Дрейф.** Строка на машину и ресурс, если проблема держится дольше порога. `behind` -
последняя желаемая ревизия не применена, `since` - время её создания (у машины без агента
тоже: ресурсы ей положены, но не доставлены). `failed` - ресурс в `failed` или
непройденная проверка (ресурс `check:<имя>`), `since` - начало отказа по журналу
применений. `silent` - агент молчит дольше порога, одна строка на машину с пустым
`resource`. Выключенные машины в дрейф не попадают. Порядок - машина, причина, ресурс.

Каждое изменение пользователей публикует **ревизию**: `users.toml` собирается заново
(одинаковые данные - одинаковые байты) и записывается в `desired_config` тем машинам из
`targets`, у которых он отличается. Агенты на машинах забирают ревизии и отчитываются в
`observed_state` (этап 2 плана, [docs/PLAN.md](docs/PLAN.md)).

API агентов на машинах (Bearer-токен машины):

| Метод | Путь | Что делает |
|---|---|---|
| `GET` | `/v1/desired?target=&rev=&wait=` | long-poll: бандл сразу, если есть ревизия новее `rev`, иначе ожидание до `wait` (до 60 с) и 304 |
| `POST` | `/v1/observed` | отчёт агента: ревизия, хеш, статус (`applied`, `failed`, `pending`, `compared`) по ресурсам и локальные проверки машины (`checks`); у машины без ресурсов список ресурсов пустой |
| `GET` | `/v1/stack/blobs/{sha256}?target=` | бинарь обновления из кэша мастера (`application/octet-stream`); только бинари из последней ревизии ресурса `stack` этой машины, иначе 404; чужой `target` - 403 |
| `POST` | `/v1/runtime` | runtime-отчёт агента раз в минуту (см. «Runtime-отчёт»); лимиты и неразумные значения - 400 (`host_invalid`, `log_period_invalid`, `log_count_invalid`), тело больше 2 МБ - 413, чужой `target` - 403, ответ 204 |

Токен машины даёт доступ только к её бандлу; в базе хранится только хеш токена.

Установка агента (одноразовый токен в пути, без Bearer; на TLS-адресе мастера и через мост выхода):

| Метод | Путь | Что делает |
|---|---|---|
| `GET` | `/v1/join/{token}` | bash-скрипт установки; токен не тратится |
| `GET` | `/v1/join/{token}/telemux` | бинарь мастера (если мастер собран под linux/amd64, иначе 503); токен не тратится |
| `POST` | `/v1/join/{token}/agent?format=env` | обмен на токен агента, один раз (повтор - 404); только мастер |

Неизвестный, просроченный или использованный токен - 404 без подробностей.

Ошибки - `{code, message}`: 4xx - ошибка запроса, 503 - недоступна база, 500 - баг.

## Агент на машине

Агент сам забирает желаемое состояние у мастера и отчитывается
([ADR 0001](docs/adr/0001-pull-reconcile.md)). Режимы:

- **compare** - сравнивает `users.toml` на диске с желаемым по смыслу (набор
  пользователей, секреты, сроки, лимиты; порядок строк и формат не важны) и
  отправляет отчёт. Файлы не меняет. С него начинается подключение машины.
- **apply** - применяет: проверяет место на диске, пишет временные файлы и сверяет
  их, сохраняет `.telemux-bak`, одним шагом заменяет `users.toml` и WEB-профили в
  `telemt.toml` (профили пересобираются из того же списка пользователей), шлёт
  SIGHUP и ждёт подтверждения telemt через `/v1/config/state`. Отказ telemt или
  отсутствие подтверждения за 12 с - оба файла возвращаются. Отвергнутая ревизия
  повторно не применяется: агент ждёт следующую. Пустой список поверх непустого и
  профилей больше `max_profiles` агент не применяет вовсе.

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

**Машина без ресурсов.** Агент ставится на каждую машину флота, в том числе на выход
без telemt и mtg (только туннель и мост для релея). Такая машина заводится без
`--resources`, у агента `TELEMUX_AGENT_USERS_FILE=none`. Раз в 5 минут (и при каждой
сверке ресурсов) агент отчитывается о себе: версия, мост, результат локальных проверок
`TELEMUX_AGENT_CHECKS` (например `bridge=http://127.0.0.1:18480/healthz`). Последний
отчёт машины хранится в `agent_state`, смена результата проверки пишется в `apply_log`
как ресурс `check:<имя>`.

Сводка по всем машинам:

```sh
telemux machines        # код, вид, когда отчитывался агент, проверки, ресурсы
```

### Runtime-отчёт

Отдельно от отчёта `observed` (раз в 5 минут) агент раз в `--runtime-every`
(по умолчанию 60 с, `0` - выключить) шлёт лёгкий снимок машины `POST /v1/runtime`.
Своим циклом: не ждёт long-poll и сверку. Не дошёл - не копится, следующий раз
соберёт свежий; мост пересылает его как есть, без очереди (адрес-реплику с 409
`not_master` мост пропускает и идёт к следующему мастеру, как агент). Старый мастер
или мост без `/v1/runtime` (404/405) - агент удваивает паузу до 30 мин и
возвращается к обычному периоду после первого принятого отчёта.

- **host** - CPU по двум замерам `/proc/stat` (между отчётами), память, `load1`,
  uptime, занятость корневой ФС, скорость интерфейса маршрута по умолчанию
  (`/proc/net/route`, `/proc/net/dev`) между отчётами;
- **telemt** (если ведётся `users.toml`) - версия (`<telemt-bin> --version`, повторно
  только при смене бинаря), `GET /v1/users` (число пользователей без `placeholder`,
  подключения), действующие - по `[access.user_expirations]` в `users.toml`,
  `GET /v1/stats/users/active-ips`, сводка `telemt.toml` (sha256, `client_keepalive`,
  `client_ack`, `tls_domain`) и медиана лимитов подключений;
- **mtg** (если ведётся `config.toml` mtg) - версия и `/stats` (подключения, трафик,
  `last_seen`, `active_ips`; у старых сборок адресов нет - пустой список);
- **log_errors** - счётчики ошибок журнала telemt (`journalctl -u <unit>`) по
  пользователю (`user=<key>`) и виду (`limit`, `handshake`, `auth`, `timeout`, `other`)
  за период с конца последнего принятого мастером отчёта (не дальше часа назад);
  журнал читается построчно, сбой `journalctl` - предупреждение в логе агента и
  строка в `telemt.error`.

В отчёт идут только пользователи с подключениями или адресами: до 5000 в продукте и до
20 адресов на пользователя. Тело больше 2 МБ агент урезает сам: сначала адреса, затем
пользователей с меньшим числом подключений, затем строки журнала - и пишет об этом в
`error` продукта. Версия бинаря (и неудача `--version`) запоминается до замены файла.
Сбой одного источника отчёт не роняет: у продукта `api_ok=false` и `error`. Мастер
хранит только последний снимок машины (`agent_runtime`) и строки пользователей
(`runtime_user_conns`): строки продукта заменяются только отчётом, где API продукта
ответил; при `api_ok=false` прежние строки остаются и устаревают по времени. Счётчики
журнала (`runtime_log_errors`) хранятся 24 часа, повтор того же периода их не
удваивает; старое чистит мастер раз в 10 минут.

**Порядок выкатки:** миграция `migrations/0007_agent_runtime.sql` → мастер (и
реплики) → мосты → агенты. Мастер на старте проверяет таблицы 0007 в своей схеме и
без них не запускается (иначе `/api/v1/machines` отвечал бы 500). Агент новее мастера
или моста не ломается: на 404/405 он только реже повторяет отчёт.

Флаги агента: `--runtime-every`, `--telemt-api` (`http://127.0.0.1:9091`), `--telemt-bin`
(`telemt`), `--telemt-config` (`/etc/telemt/telemt.toml`), `--mtg-bin` (`mtg`),
`--journal-unit` (`telemt`), `--bridge-url` (`TELEMUX_AGENT_BRIDGE_URL`, только на выходе с
мостом: сводка релеев моста в поле `bridge` отчёта); переменные - в
[deploy/agent.env.example](deploy/agent.env.example).

### Основной telemt.toml (ресурс telemt.config)

Бот готовит основной `telemt.toml` каждой машины (общая статика, `announce_ip` из своей
базы, `include` на users.toml) и публикует его в telemux:
`PUT /api/v1/targets/{code}/resources/telemt.config`. Ревизии у каждой машины свои.

В одном файле три хозяина, поэтому агент файл **собирает**, а не кладёт как есть:

| Часть | Хозяин | Что внутри |
|---|---|---|
| база | ресурс `telemt.config` (бот) | всё общее: таймауты, censorship, `announce_ip`, `include` |
| машинная часть | файл на машине `/etc/telemt/telemt.local.toml` (человек) | WEB-режим (`[web]`, `[[web.vhosts]]`), локальный listener и порт (8443, 8453), `mask_port`, свои каталоги `/var/lib/...`, `public_addr` - всё, что правится руками при переездах |
| WEB-профили `[[web.vhosts.profiles]]` | ресурс `telemt.users` | собираются из users.toml машины, параметры (`secret_mode`, лимиты) - из текущих профилей машины |

Машинная часть сильнее базы: ключ `[таблицы]` заменяет одноимённый ключ базы на его
месте (новый - дописывается в конец таблицы), `[[массив]]` заменяет группу базы целиком
(например, все `[[server.listeners]]`), таблица, которой в базе нет, - дописывается в
конец. Удалять ключи базы машинная часть не умеет. Профилей нет ни в базе (мастер
отвечает 400), ни в машинной части (агент не применяет). `telemt.users` трогает только
блок профилей, поэтому оба ресурса сходятся к одному тексту и не перетирают друг друга.

Применение: сборка и проверки (WEB-режим на машине не теряется, `include` ведёт на
users.toml агента, API telemt не выключен) → место → временный файл со сверкой → бэкап
`telemt.toml.telemux-cfg-bak` → замена → SIGHUP → telemt сам проверяет и принимает конфиг
(`/v1/config/state`). Если часть полей telemt применит только рестартом
(`process_deferred_paths`: порт, listener) - рестарт, затем проверка, что API подтвердил
конфиг, юнит активен и systemd не перезапускал его ~8 с. Рестарт делается только после
того, как telemt признал конфиг валидным. Любой сбой - возврат прежнего файла (после
рестарта - рестартом). Отвергнутая ревизия не повторяется, ждётся новая.

Включение на машине (только вместе с `telemt.users`):

```sh
telemux target --code pr3 --kind relay --resources telemt.users,telemt.config,mtg.config
# agent.env: TELEMUX_AGENT_MAIN_CONFIG=1 (сначала в режиме compare)
```

Перевод машины с ручными правками - сначала вывести машинную часть:

```sh
telemux desired --target pr3 > /tmp/base.toml          # на мастере: опубликованная база
telemux telemt-local --base /tmp/base.toml > telemt.local.toml   # на машине, по /etc/telemt/telemt.toml
```

Код 1 и предупреждения - на машине удалены ключи или секции базы, машинной частью это не
выразить: правку надо перенести в генератор бота. В режиме compare агент в отчёте
называет секции, которыми собранный конфиг отличается от файла на машине; пустое
расхождение - можно включать apply.

Переменные агента: `TELEMUX_AGENT_MAIN_CONFIG` (`1` - вести, по умолчанию выключено),
`TELEMUX_AGENT_CONFIG_FILE` (`/etc/telemt/telemt.toml`), `TELEMUX_AGENT_LOCAL_CONFIG`
(`/etc/telemt/telemt.local.toml`, файла может не быть), `TELEMUX_AGENT_CONFIG_STATE_URL`
(`http://127.0.0.1:9091/v1/config/state`).

### mtg-multi

Агент ведёт и `config.toml` mtg-multi (ресурс `mtg.config`, текст готовит бот):
`TELEMUX_AGENT_MTG_FILE=/etc/mtg-multi/config.toml`. Изменились только `[secrets]` -
SIGHUP, иначе рестарт юнита; применено, когда `secrets_sha256` в `/stats` mtg совпал с
желаемым набором; иначе прежний конфиг возвращается. На машине только с mtg -
`TELEMUX_AGENT_USERS_FILE=none`.

mtg внутри мастер-панели 3x-ui ведёт сам мастер telemux (машина вида `panel`):

```sh
telemux target --code xui-ps7 --kind panel --resources xui.clients \
  --settings inbound_id=7,node_id=3,port=8767,node_code=ps7
```

и переменные `TELEMUX_XUI_URL`, `TELEMUX_XUI_TOKEN`, `TELEMUX_XUI_CERT_SHA256` у `serve`.

### Сертификаты WEB (ресурс tls.web)

TLS доменов `webN` (WEB-режим telemt и mtg) на релеях держит nginx. Сертификаты
выпускает и продлевает **мастер** по ACME DNS-01 через Cloudflare, токен Cloudflare есть
только у мастера; агент релея кладёт файлы и перезагружает nginx
([ADR 0004](docs/adr/0004-tls-web.md)).

Список доменов релея - `targets.settings.web_domains`, через запятую. Только имена
(буквы, цифры, дефис), без звёздочек и IP; регистр и повторы нормализуются, мусор -
ошибка при записи машины. Домен на двух релеях выпускается один раз.

Мастер (переменные `TELEMUX_ACME_*`, см. «Конфигурация» и
[deploy/telemux.env.example](deploy/telemux.env.example)) раз в час проходит по доменам
включённых релеев и выпускает сертификат, если его нет или до конца меньше
`TELEMUX_ACME_RENEW_BEFORE` (по умолчанию 30 дней). Неудача - `last_error` в `tls_certs`
и WARN в лог; домен пробуется снова не раньше чем через 6 часов (лимиты Let's Encrypt).
Затем каждой машине с назначенным `tls.web` публикуется ресурс - JSON
`{"<домен>": {"cert": "...", "key": "..."}}` со всеми её доменами, у которых есть
сертификат; новая ревизия - только при изменении содержимого. Аккаунт ACME (ключ) хранится
в базе (`acme_account`, свой на каждый каталог ACME), выпуск - только на мастере. Без
`TELEMUX_ACME_EMAIL` и `TELEMUX_ACME_CF_TOKEN_FILE` выпуск выключен, остальное работает
как раньше. Нужна миграция [migrations/0009_tls_certs.sql](migrations/0009_tls_certs.sql):
с включённым выпуском без неё мастер не стартует.

Агент (`TELEMUX_AGENT_TLS_WEB=1`, каталог `TELEMUX_AGENT_TLS_DIR`, по умолчанию
`/etc/nginx/certs`): для каждого домена ресурса - `<каталог>/<домен>.pem` (полная цепочка)
и `<каталог>/<домен>.key` (0600). До записи проверяет имя домена, что сертификат и ключ -
пара и что сертификат выписан на этот домен. Меняются только отличающиеся файлы: временный
файл со сверкой → бэкап `.telemux-bak` → замена → `nginx -t` → `systemctl reload nginx`.
Провал `nginx -t` или перезагрузки - прежние файлы возвращаются (новые домены удаляются),
статус `failed` с причиной. Домены, которых нет в ресурсе, агент не трогает и не удаляет.
В режиме compare отчёт называет домены, чей сертификат или ключ отличается (по sha256)
или отсутствует; файлы не меняются.

Юнит агента ([deploy/telemux-agent.service](deploy/telemux-agent.service)) - с
`ProtectSystem=strict`: писать можно только в `ReadWritePaths`. Там `/etc/nginx/certs`
(сертификаты) и `/var/log/nginx` (`nginx -t` открывает журналы nginx на запись). Каталог
сертификатов должен существовать до старта юнита (скрипт установки агента создаёт его, если
есть `/etc/nginx`); другой `TELEMUX_AGENT_TLS_DIR` - добавить в `ReadWritePaths` через
`systemctl edit telemux-agent`.

Перевод релея:

```sh
# 1. На мастере: назначить ресурс и задать домены (--resources - полный список ресурсов машины)
telemux target --code pr2 --kind relay --resources telemt.users,tls.web \
  --web-domains web2.example.com,web6.example.com
# 2. Дождаться выпуска: GET /api/v1/certs - not_after заполнен, last_error пуст
# 3. На релее: ссылки nginx на новые файлы, проверить конфиг
#    ssl_certificate     /etc/nginx/certs/web2.example.com.pem;
#    ssl_certificate_key /etc/nginx/certs/web2.example.com.key;
# 4. agent.env: TELEMUX_AGENT_TLS_WEB=1 в режиме compare, systemctl restart telemux-agent;
#    в отчёте (observed_state, панель) - какие домены отличаются
# 5. TELEMUX_AGENT_MODE=apply, systemctl restart telemux-agent: файлы на месте, nginx перезагружен
# 6. Выключить старый выпуск на релее: certbot (systemctl disable --now certbot.timer,
#    удалить renewal-конфиги доменов) или acme.sh (acme.sh --remove -d <домен>, крон);
#    токен Cloudflare с релея удалить
```

Пока nginx ссылается на старые файлы certbot или acme.sh, новые сертификаты лежат рядом
без эффекта - шаг 3 можно делать до или после включения агента, но до выключения старого
выпуска.

### Мастер для машин-выходов по HTTPS

Машины-выходы видят мастера напрямую, но в бандлах секреты пользователей, поэтому их
агенты и мосты ходят к мастеру только по TLS с закреплённым сертификатом:

```sh
telemux tls-cert                       # на мастере: сертификат + отпечаток
# telemux.env: TELEMUX_TLS_LISTEN=0.0.0.0:8443 (фаервол - только IP машин-выходов)
# агенты и мосты: https://<мастер>:8443 и TELEMUX_AGENT_MASTER_PIN / TELEMUX_BRIDGE_MASTER_PIN
```

На TLS-адресе только `/v1/*` и `/healthz`: панель и API бота остаются на локальном
адресе. Сертификат один на все машины управления - при смене мастера отпечаток у
агентов не меняется. Отпечаток проверяется до отправки запроса: чужому серверу токен
машины не уходит.

### Обновление telemt и mtg-multi (ресурс stack)

Мастер публикует машине ресурс `stack`: какая версия telemt и mtg-multi должна стоять, с
sha256 и размером бинаря ([ADR 0008](docs/adr/0008-stack-updates.md)). Агент ведёт ресурс
всегда и сравнивает с версией на машине (`--version`, как в runtime-отчёте; ядро и суффикс
должны совпасть, ведущая `v` не важна). Продукта на машине нет (`users.toml` или
`config.toml` mtg - `none`) - статус `failed`.

- compare - только отчёт: `telemt: на машине 3.5.16, нужно 3.5.17`.
- apply - агент запускает вне своей песочницы (`ProtectSystem=strict`, в `/usr/local/bin`
  ему писать нельзя) временный юнит:
  `systemd-run --wait --pipe --collect --quiet --unit=telemux-stack-<продукт>-<ревизия> -p EnvironmentFile=/etc/telemux/agent.env -p RuntimeMaxSec=540 telemux stack-apply ... --bin <путь>`
  (путь бинаря - тот же, по которому агент берёт версию). Код выхода 0 - `applied`, иначе
  `failed` с хвостом вывода. Ревизия применяется один раз: перепроверка той же ревизии
  только сравнивает и повторяет прежний итог (версию, поставленную после этого вручную,
  агент не откатывает); новая ревизия - новый запуск.

`telemux stack-apply --product telemt|mtg --sha256 <s> --version <v> --size <n>` (от root):
берёт бинарь у мастера (`GET /v1/stack/blobs/{sha256}`, адреса и мосты из
`TELEMUX_AGENT_MASTERS`, токен агента; первый ответивший), сверяет размер, sha256 и
`--version` нового файла, кладёт копию текущего в `<бинарь>.bak`, атомарно заменяет и
перезапускает юнит (всё обновление - не дольше 8 мин). Если нужная версия уже на диске,
но сервис не жив (прошлый запуск убили посреди замены) - перезапуск и проверка, при
провале - откат из `.bak`. Проверка - до 30 с: юнит `active` непрерывно 5 с, systemd не
перезапускал его сам, API продукта отвечает (telemt - `TELEMUX_AGENT_TELEMT_API` +
`/v1/health`, mtg - `TELEMUX_AGENT_MTG_STATS_URL`). Не поднялся - `.bak` на место,
перезапуск, код выхода 1. Успех - строка `telemt: 3.5.16 -> 3.5.17`.

Бинарь и юнит - из того же `agent.env`: `TELEMUX_AGENT_TELEMT_BIN` (`telemt`),
`TELEMUX_AGENT_TELEMT_UNIT` (`telemt`), `TELEMUX_AGENT_MTG_BIN` (`mtg-multi`),
`TELEMUX_AGENT_MTG_UNIT` (`mtg-multi`); имя без пути ищется в `PATH`. Путь env-файла для
временного юнита - `TELEMUX_AGENT_ENV_FILE` (по умолчанию `/etc/telemux/agent.env`).

## Мост на машине-выходе

Релей ходит к мастеру только через туннель своей пары ([ADR 0001](docs/adr/0001-pull-reconcile.md)).
На машине-выходе работает `telemux bridge`: принимает запросы агента релея из
туннеля и пересылает их мастеру с тем же токеном релея.

- Последний бандл каждого токена мост держит в памяти. Если мастер недоступен,
  мост отдаёт бандл из кэша (заголовок `X-Telemux-From-Cache: 1`), но только тому
  токену, который мастер уже принимал; отозванный мастером токен кэш теряет.
- Отчёт релея при недоступном мастере мост принимает (`202`) и досылает раз в 30 с.
- Установку агента на релей (`/v1/join/*`: скрипт, бинарь, обмен токена) мост
  пересылает мастеру без токена агента и без кэша; бинарь идёт потоком.
- У каждого релея два моста (выход telemt-сервера и выход mtg-сервера): агент
  релея перечисляет оба в `TELEMUX_AGENT_MASTERS`, каждый через свой туннель -
  запись `адрес|socks5://127.0.0.1:<порт входа туннеля>`.

Мост знает свой релей ([ADR 0009](docs/adr/0009-relay-via-exit.md)):

- С кодом выхода (`TELEMUX_BRIDGE_CODE`, флаг `--code`) мост ставит в каждый запрос к
  мастеру заголовок `X-Telemux-Bridge: <код>` (присланный клиентом заголовок мастеру не
  уходит); мастер записывает его в `relay_via` машины вида `relay`.
  Без кода заголовка нет, мост работает как раньше; старый мастер заголовок не читает.
- Мост ведёт сводку релеев, которые через него ходят: время последнего запроса, число
  запросов, ответил ли мастер (`master_ok`), последняя ошибка мастера. Код релея мост
  берёт только из ответа мастера на `/v1/desired` (`target` бандла или 304 на `target`
  запроса); токен, которого мастер не признал, в сводку не попадает. Мастер недоступен -
  запрос всё равно учитывается (релей жив), `master_ok=false`. `last_error` - класс ошибки
  без адресов и query (`мастер не ответил: таймаут`, `мастер ответил сбоем: HTTP 502`).
  Релей, который не ходил через мост 24 ч, из сводки забывается.
- `GET /v1/relays` на адресе моста - сводка `{bridge, relays: [{code, last_seen, requests,
  master_ok, last_error}]}` по коду, без токенов и их хешей. Её раз в минуту забирает агент
  выхода (`TELEMUX_AGENT_BRIDGE_URL`, обычно `http://127.0.0.1:18480`) и шлёт мастеру в
  runtime-отчёте (поле `bridge`; мост не ответил за 3 с - `bridge.error`, остальной отчёт
  уходит как обычно).

Мост слушает только loopback (`127.0.0.1:18480` по умолчанию). Туннель настраивается
вручную при подключении пары: на релее - локальный вход, который маршрутизация
xray отправляет в обратный выход своей пары; на машине-выходе - разрешение
доставки на loopback-порт моста (по умолчанию xray не пускает приватные адреса).
Настройки - [deploy/bridge.env.example](deploy/bridge.env.example), юнит -
[deploy/telemux-bridge.service](deploy/telemux-bridge.service).

## Релей через свою ноду

Состояние релеев в РФ мастер узнаёт по цепочке **мастер -> выход -> релей**
([ADR 0009](docs/adr/0009-relay-via-exit.md), [спецификация](docs/specs/relay-via-exit.md)).
Своя нода релея - выход с `relay_code` = код релея (пары берутся только из туннелей,
карта серверов на них не влияет). Статус своей ноды в паре (`relay_status`):

- `exit_down` - агент выхода молчит дольше 10 минут: о релее пара ничего не утверждает;
- `ok` - выход жив, и релей ходил через его мост не позже 3 минут назад (по сводке моста
  из runtime-отчёта выхода не старше 10 минут и с кодом этого выхода, или по
  `relay_via_at` релея);
- `relay_silent` - выход жив, а релей через его мост молчит дольше 3 минут.

Релей `down` (`relay_down` в паре, событие `down`) - только если его агент молчит дольше
10 минут и ни одна своя нода не видела его через мост дольше 3 минут. `foreign_bridge` -
последний отчёт релея пришёл через мост выхода не из его пары. У релея без своих нод
статус пары `no_exit`. Выключенный выход в расчёте `relay_down` не участвует. Статусы в API
и в детекторе считаются от одного времени - `now()` базы.

Чтобы мост каждой своей ноды видел релей, агент с несколькими адресами мастера раз в
минуту своим тикером (независимо от runtime-отчёта) спрашивает `GET /v1/desired?wait=0s`
через каждый адрес - параллельно, не дольше 5 с на адрес (иначе мост второй ноды, через
которую агент не ходит, давал бы `relay_silent`).

Тревоги - в той же ленте `GET /api/v1/events`, только на переходах: `relay_silent`
(`machine` - релей, `exit` - выход), `relay_ok` (пара вернулась в `ok` после
`relay_silent`), `foreign_bridge` (`exit` - чужой выход), `relay_no_exit`. Пара с
`exit_down` событий не даёт; выключенные машины и релеи без агента - тоже. Пока релей
`down`, пары по нему молчат: одно падение - одно событие `down`.

Включение на выходе:

```bash
# /etc/telemux/bridge.env
TELEMUX_BRIDGE_CODE=<код выхода>
# /etc/telemux/agent.env
TELEMUX_AGENT_BRIDGE_URL=http://127.0.0.1:18480
systemctl restart telemux-bridge telemux-agent
```

Порядок выкатки: миграция `migrations/0014_relay_via.sql` (мастер без неё не запускается) ->
мастер -> мосты и агенты выходов -> агенты релеев (проба всех мостов). Пока мост выхода
без кода, а агент выхода без адреса моста, пары этого выхода видны как `relay_silent`
(кроме той, через которую релей отчитывается сам); новый мост и агент со старым мастером
работают, поэтому их можно выкатить и раньше мастера.

## Добавление машины

Одна команда на новой машине, как в 3x-ui, но агент сам ходит к мастеру (pull):

1. В панели «Добавить машину» или `POST /api/v1/targets`:

   ```json
   {"code": "ps9", "kind": "exit", "relay_code": "pr2", "address": "<IPv4 выхода>",
    "bridge_socks": "socks5://127.0.0.1:10873", "resources": ["telemt.users"]}
   ```

   Код - `^[a-z][a-z0-9-]{1,31}$`. У выхода обязательны существующий релей, публичный
   IPv4 и `bridge_socks` - socks-вход на релее, через который релей ходит в этот выход
   (`socks5://127.0.0.1:<порт>`). Ресурсы - `telemt.users` и `mtg.config`. Ответ - команда:

   - выход: `curl -fsSLk --pinnedpubkey 'sha256//<ключ сертификата мастера>' https://<мастер>:18443/v1/join/<токен> | sudo bash` -
     напрямую на мастер, TLS проверяется по ключу сертификата (как отпечаток у агентов);
   - релей: `curl -fsSL -x socks5://127.0.0.1:<порт> http://<адрес выхода>:18480/v1/join/<токен> | sudo bash` -
     через мост своего первого выхода (по коду), у которого заданы адрес и socks-вход.

   Новый релей заводится раньше своих выходов, поэтому команда ему не выдаётся
   (`command` пустая): добавьте выход, затем `POST /api/v1/targets/<код релея>/join`.
2. Выполнить команду под root на новой машине (Linux amd64, systemd). Токен живёт 1 час
   и тратится один раз; новая команда (`POST .../join`) гасит прежнюю.
3. Скрипт: скачивает бинарь `telemux` тем же путём (`/usr/local/bin/telemux` через
   временный файл), меняет одноразовый токен на токен агента, кладёт
   `/etc/telemux/agent.token` и `/etc/telemux/agent.env` (600; прежний `agent.env`
   сохраняется в `agent.env.bak-<дата>`), ставит юнит `telemux-agent`, запускает его и
   печатает статус и журнал. В `agent.env` - код машины, адреса мастера (у выхода -
   `TELEMUX_AGENT_PUBLIC_URL` и отпечаток сертификата, у релея - мосты всех его выходов
   `http://<адрес>:18480|<socks>`), файлы ресурсов (`none`, если ресурс не назначен).
4. Агент стартует в режиме **compare** - только сравнивает. Убедиться по панели или
   `telemux machines`, что сравнение сходится, и перевести вручную:
   `TELEMUX_AGENT_MODE=apply` в `/etc/telemux/agent.env`, `systemctl restart telemux-agent`.

Скрипту не нужны `jq` и `python`: обмен вызывается с `?format=env`, и мастер отвечает
текстом - первая строка токен агента (скрипт проверяет, что это 64 hex), остальное -
готовый `agent.env`. Все значения в скрипте и команде проверены мастером (код, hex-токен,
https-адрес, IPv4, socks на loopback, base64-ключ) и подставлены в одинарных кавычках.

**Один раз на мастере:**

```sh
telemux tls-cert                              # если ещё нет сертификата API агентов
# /etc/telemux/telemux.env:
#   TELEMUX_TLS_LISTEN=0.0.0.0:18443
#   TELEMUX_AGENT_PUBLIC_URL=https://<внешний адрес мастера>:18443
#   TELEMUX_AGENT_FW_CHAIN=TELEMUX-AGENTS     # необязательно
```

Без `TELEMUX_AGENT_PUBLIC_URL` (или без сертификата для выхода) добавление машины
отвечает 409 с объяснением и ничего не заводит.

С `TELEMUX_AGENT_FW_CHAIN` мастер при добавлении выхода сам открывает его адрес на
TLS-порту агентов (порт - из `TELEMUX_TLS_LISTEN`): `iptables -w -C <цепочка> -s <IPv4>
-p tcp --dport <порт> -j ACCEPT`, при отсутствии - `-I`. Результат - в поле `firewall`
ответа: `opened`, `not_configured` (цепочка не задана), `failed` (подробности в журнале
мастера; машина и команда при этом выданы - откройте адрес вручную), `not_needed` (релей).
Такие правила живут только до перезагрузки, поэтому демон с заданной цепочкой при старте и
затем раз в 10 минут проходит по включённым выходам с адресом и дописывает недостающие
разрешения (уже существующие не трогает, сбои - в журнал с уровнем WARN).
Цепочку создают один раз руками и подключают перед правилом, которое закрывает порт:

```sh
iptables -N TELEMUX-AGENTS
iptables -I INPUT -p tcp --dport 18443 -j TELEMUX-AGENTS
iptables -A INPUT -p tcp --dport 18443 -j DROP
# сохранить правила способом дистрибутива (netfilter-persistent save и т.п.)
```

`telemux serve` должен работать от root (как `deploy/telemux.service`), иначе
iptables вернёт ошибку и `firewall` будет `failed`.

**Мосты на выходах.** Команда релея идёт через мост его выхода, поэтому мосты нужно
обновить до версии, которая пересылает `/v1/join/*` (скрипт, бинарь потоком, обмен
токена; перебор мастеров тот же). Старый мост ответит 404 на команду релея.

## Серверы

«Сервер N», который видит клиент: страна и флаг, движок (`telemt` или `mtg`, после создания
не меняется), релей в РФ (`relay_code`, машина вида `relay`) и машина за рубежом
(`node_code`, вида `exit`), состояние (`active`, `maintenance` - «на профилактике» без
ссылки, `test` - скрыт от клиентов, `disabled` - выключен везде), домены маскировки,
лимиты пользователя, настройки движка и точки входа - одна строка на ссылку (`ee`, `dd`,
`web`: хост, порт, домен ee-секрета, через релей или напрямую, подпись, порядок).
Схема - `migrations/0010_servers.sql`, решение - [ADR 0005](docs/adr/0005-servers.md).

Бот пока берёт ссылки и точки входа из своей базы, а конфиги машин (`telemt.config`,
`mtg.config`) из описания сервера уже умеет собирать telemux - см. «Сборка конфигов серверов».

Настройки движка (`settings`) - только ключи из каталога полей
(`GET /api/v1/servers/fields`), значения проверяются по типу и пределам, bool хранится как
`"0"`/`"1"`. Ошибки ввода - 400 с кодом: `unknown_machine` (нет машины или не того вида),
`engine_immutable`, `unknown_setting`, `bad_setting`, `bad_endpoint` (вид, хост, порт,
`tls_domain` для `ee` обязателен, повтор точки входа), `batch_too_large`. Машину, на которую
ссылается сервер, удалить нельзя. Без миграции 0010 ручки серверов выключены (WARN при старте).

Импорт из файла (тот же формат, что у `PUT /api/v1/servers` - массив серверов, до 100):

```sh
telemux servers import --db "$DATABASE_URL" servers.json   # на мастере; печатает новых/обновлено
```

Импорт идемпотентен: повтор с тем же файлом обновляет те же серверы и точки входа. Любая
ошибка (машины нет, движок другой, плохая настройка) - не меняется ничего.

### Сборка конфигов серверов

Решение - [ADR 0006](docs/adr/0006-server-configs.md), схема - `migrations/0011_server_render.sql`
(без неё ручки серверов и цикл сборки выключены, WARN при старте).

telemux собирает из описания сервера основной `telemt.toml` (ресурс `telemt.config`) - для
машины за рубежом (кроме `node_retired`) и релея - и `config.toml` mtg-multi (`mtg.config`) -
для машины за рубежом (кроме доставки через панель, `BACKEND=xui`) и релея в режиме
`RELAY_MODE=local-mtg`. Текст - байт в байт как у сборщиков бота (`internal/render`, эталоны
в `internal/render/testdata` получены прогоном кода бота). Настройки - из `settings` сервера,
отсутствующие - по умолчанию каталога; домен секретов mtg - `tls_domain` сервера, домен WEB -
первая включённая точка входа `web`; `[secrets]` - включённые пользователи со сроком в
будущем (и без срока) плюс прежний ключ (`_legacy`) до его срока.

`announce_ip` в `telemt.config` - адрес машины, `targets.settings.address`: у выхода он задан
при добавлении, релею его нужно задать (`POST /api/v1/targets` с `address`, правка машины или
`telemux target ... --settings address=<IP>`). Нет адреса - ошибка `no_address`.

Цикл на мастере (раз в минуту и сразу после правки сервера или пользователей) собирает
кандидатов всех не выключенных серверов и сравнивает с последней ревизией ресурса машины.
Результат - `GET /api/v1/servers/{no}/render`: sha кандидата и текущей ревизии, совпадение,
ошибка (`no_address`, `not_assigned` - ресурс не назначен машине, `machine_disabled`,
`no_tls_domain`, `bad_setting`, `shared_machine` - ресурс машины собирают несколько серверов)
и unified diff «текущий → кандидат», где значения `[secrets]` и `[access.users]` заменены на
`<secret>`. В списке серверов - `config_source` и сводка `render: {rows, equal, errors, rendered_at}`.

Переключение - по серверу:

```sh
# посмотреть расхождения
curl -s -H "Authorization: Bearer $TOKEN" https://<мастер>/api/v1/servers/1/render
# переключить: только если всё совпало, иначе 409 render_mismatch; force - при расхождении
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -d '{"source":"telemux"}' https://<мастер>/api/v1/servers/1/config-source
# откат - всегда
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -d '{"source":"bot"}' https://<мастер>/api/v1/servers/1/config-source
```

При `config_source=telemux` цикл публикует кандидата сам (ревизия - только при изменении), а
`PUT /api/v1/targets/{code}/resources/{telemt.config|mtg.config}` для таких машин отвечает 409
`managed_by_telemux` - бот считает это пропуском. Перед переключением: адреса релеев в
`targets.settings.address`, ресурс назначен машине (`targets.resources`), импорт серверов из
бота свежий (`telemux servers import`), сравнение без расхождений.

## Обновление telemt и mtg-multi

Версии берутся из релизов GitHub ([ADR 0008](docs/adr/0008-stack-updates.md)), схема -
`migrations/0013_stack.sql`. Без миграции ручки и цикл выключены (WARN при старте), остальное
работает.

| Продукт | Репозиторий (env мастера) | Архив релиза | Контрольная сумма |
|---|---|---|---|
| `telemt` | `TELEMUX_STACK_TELEMT_REPO`, `telemt/telemt` | `telemt-x86_64-linux-musl.tar.gz` (arm64 - `telemt-aarch64-linux-musl.tar.gz`) | `<архив>.sha256` |
| `mtg` | `TELEMUX_STACK_MTG_REPO`, `dolonet/mtg-multi` | `mtg-multi-<версия>-linux-<amd64\|arm64>.tar.gz` | `mtg-multi-<версия>-checksums.txt` |

**Проверка релизов.** Мастер при старте и раз в 6 часов узнаёт последний релиз каждого
продукта (не черновик и не pre-release) и хранит его в `stack_releases`; по кнопке -
`POST /api/v1/stack/check`, не чаще раза в минуту. GitHub не ответил - прошлый релиз
остаётся, текст ошибки - в `error`. `TELEMUX_GITHUB_TOKEN` нужен только против лимита
запросов к API.

**Сравнение версий** - по ядру `X.Y.Z`, ведущая `v` отбрасывается. Своя сборка
(`custom_build`) - версия с суффиксом после ядра (`1.15.0-webalias`) или неразборчивая;
`update_available` - ядро релиза строго больше ядра на машине, поэтому `1.15.0-webalias` при
релизе `1.15.0` обновлением не считается. Продукта на машине нет (агент его не присылает) -
поле продукта `null`. Архитектура машины - `host.arch` из runtime-отчёта, по умолчанию amd64.

**Обновление** - `POST /api/v1/machines/{code}/update`:

```sh
# последний релиз telemt
curl -s -X POST -H "Authorization: Bearer $TOKEN" -d '{"product":"telemt"}' \
  https://<мастер>/api/v1/machines/pr1/update
# конкретная версия mtg-multi поверх своей сборки - только явно
curl -s -X POST -H "Authorization: Bearer $TOKEN" -d '{"product":"mtg","version":"1.15.0","force":true}' \
  https://<мастер>/api/v1/machines/pr1/update
```

1. Проверки: 400 `unknown_product`, `unknown_version` (нет такого релиза или его версия не
   разбирается: тег ищется как есть, с `v` и без); 404 - нет машины; 409 `machine_disabled`,
   `product_missing` (агент продукт не присылает), `update_in_progress` (на машине ещё идёт
   обновление любого продукта - одно за раз, `force` не помогает), `asset_missing` (в релизе
   нет файла под архитектуру машины), `custom_build` и `not_newer` (своя сборка или версия не
   новее - нужен `"force": true`).
2. Мастер скачивает архив (до 100 МБ, одна загрузка за раз; уже скачанный архив повторно не
   качается), сверяет SHA256 с контрольной суммой релиза, достаёт бинарь (по имени, в любом
   подкаталоге архива) и кладёт его в `TELEMUX_STACK_DIR` под именем = SHA256 бинаря. Не
   скачалось или сумма не сошлась - 503 `release_unavailable`, причина - в журнале мастера.
   Подлинность релиза проверяется только контрольной суммой из того же релиза, без подписи.
3. Мастер публикует машине новую ревизию ресурса `stack` - JSON с одним обновляемым продуктом
   `{version, sha256, size}` - и назначает ресурс машине (`targets.resources`). Ответ - 202 с
   `revision` и `status: pending`.
4. Агент (режим apply) берёт бинарь `GET /v1/stack/blobs/{sha256}?target=<машина>`, заменяет
   его с копией `.bak`, перезапускает сервис и проверяет его; не поднялся - возвращает прежний.

Статус обновления (`update.status` в `GET /api/v1/stack`): `pending`, пока отчёт агента по
ресурсу `stack` не дошёл до ревизии обновления; затем `applied` или `failed` с причиной
агента. `failed` также, если агент в режиме compare (бинарь не меняет) или не сообщил
результат за 30 минут (например, старый агент без поддержки ресурса). Провал применения
попадает в ленту тревог как `apply_failed`.

Ресурс `stack` - разовая команда: после итога мастер той же транзакцией публикует новую
ревизию без этого продукта (продуктов не осталось - `{}`). Агент при перепроверке не ставит
бинарь снова поверх собранного вручную, а машина, вернувшаяся после таймаута, не применит
устаревшее обновление. Каталог кэша на мастере создаётся сам; бинарь есть только в кэше
мастера, который его скачал: реплика на запрос бинаря отвечает 409 `not_master`, новый мастер
после смены - 404, обновление нужно повторить.

## Машины: включение, проверка связи, нагрузка, тревоги

Машины ведутся как ноды 3x-ui ([ADR 0007](docs/adr/0007-machines-ops.md)), схема -
`migrations/0012_machine_ops.sql`. Без миграций 0010 и 0012 эти ручки, история нагрузки и
тревоги выключены (WARN при старте), остальное работает.

**Включение и выключение** - `PUT /api/v1/targets/{code}` с `{"enabled": false}`, только
мастер. Выключенной машине не публикуются новые ревизии (`telemt.users` её пропускает,
публикация ресурса бота - 409 `machine_disabled`), её нет в тревогах. Агент продолжает
отчитываться (observed, runtime), а на `/v1/desired` получает 304. При включении мастер
сразу публикует `telemt.users` машинам, у которых он устарел; `telemt.config` и
`mtg.config` бот публикует заново сам. В том же запросе можно поправить `settings`: они
сливаются с текущими, проверка - как при добавлении (выход - `address`, `bridge_socks`;
релей - `address`, `web_domains`; вход панели - `inbound_id`, `node_id`, `port`,
`node_code`), чужой ключ или неверное значение - 400 `bad_setting`.

**Удаление** - `DELETE /api/v1/targets/{code}`: только выключенную (иначе 409
`machine_enabled`) и только если на неё не ссылаются серверы, выходы релея или входы
панели (иначе 409 `machine_in_use`, номера и коды - в `message`). Уходят токены агента,
желаемое и применённое состояние, журнал, runtime и история нагрузки; события тревог
остаются до истечения срока хранения.

**Проверка связи** - `POST /api/v1/machines/{code}/check`, только мастер (проверка идёт с
него): свежесть агента (`fresh` - отчёт не старше порога свежести, `silent` - молчит дольше
10 минут) и каждая включённая точка входа серверов машины, кроме выключенных серверов:
через релей - на релее, напрямую - на машине за рубежом. Для точки - TCP к host:port, для
`web` - к host:443 и TLS-рукопожатие с SNI=host с проверкой сертификата (`not_after`,
`days_left`). 5 с на точку, до 8 параллельно, всего не дольше 20 с; ошибки сети - в
ответе (`ok: false`, `error`), а не 5xx.

**История нагрузки.** Каждый runtime-отчёт агента (раз в минуту) пишет строку в
`machine_metrics`: CPU, память, диск (%), load, приём и отдача (бит/с), подключения и
пользователи онлайн. Хранение - 8 дней, чистит цикл чистки runtime (раз в 10 минут).
`GET /api/v1/machines/{code}/metrics` усредняет по шагу (границы шагов не зависят от
`from`), не больше 2000 точек.

**Тревоги.** Мастер раз в 30 с пишет события в `machine_events`: `down` - включённая
машина с агентом молчит дольше 10 минут (порог тот же, что в обзоре и списке машин),
`up` - вернулась, `apply_failed` - агент сообщил провал применения ревизии ресурса. Одно
событие на переход и одно на ревизию; машина, где агента ещё не ставили, не молчит. Бот
читает ленту `GET /api/v1/events?after_id=<последний id>` и пересылает админу. Хранение
событий - 30 дней. Релей `down` только если молчит и через свои ноды; тревоги по парам
«релей - своя нода» - в разделе «Релей через свою ноду».

## Веб-панель

Вшита в бинарь, открывается на адресе демона (`--listen`, по умолчанию `:8080`). Вход - по
`TELEMUX_ADMIN_USER` / `TELEMUX_ADMIN_PASSWORD`; без пароля панель открыта (только за фаерволом).
Изменения принимает только мастер: на остальных узлах панель работает на просмотр и показывает
адрес мастера.

| Раздел | Что показывает |
|---|---|
| Обзор (главная) | плитки: машины (всего, агент в сети, молчат), подключения и пользователи онлайн, пары в порядке и с проблемами, сертификаты, которые истекают меньше чем через 14 дней; ниже - машины компактной таблицей (статус, CPU, память, диск, load, аптайм, подключения, онлайн, версии telemt, mtg и агента), клик по строке - карточка машины |
| Пары | релей и его выходы: сводный статус, версия агента, мост, «видели N назад», ревизии желаемая и применённая, проверки; клик - история применений; «Команда установки» |
| Подключения | сводка по машинам и продуктам (подключений, онлайн, ответил ли API, свежесть) и пользователи онлайн: машина, продукт, подключения, активные IP (раскрываются списком), трафик mtg, активность; фильтры по машине, продукту, минимуму подключений, поиск по пользователю; обновление раз в 15 секунд |
| Машины | карточки «всего / в сети / не в сети», машины флота с runtime-статусом, нагрузкой, аптаймом, подключениями и версиями; «Добавить машину», «Команда установки»; клик - карточка машины с вкладками «Состояние», «Ресурсы и ревизии», «Подключения», «Ошибки журнала» (24 ч), «История применений», «Сертификаты»; ниже - узлы управления кластером |
| Пользователи | список с серверной пагинацией, онлайн и подключения сейчас по всем машинам, создание, вкл/выкл, продление, удаление |
| Дрейф | только машины, где желаемое не совпадает с применённым дольше порога |
| Сертификаты | WEB-домены релеев: машина, назначен ли `tls.web`, срок, выпуск, последняя попытка и ошибка; меньше 14 дней до конца срока - строка подсвечена |
| Синк | статус последней синхронизации пользователей на ноды и запуск вручную |
| API | swagger-ui по `GET /api/openapi.json` |

Статус машины по runtime-отчёту агента: «в сети» - свежий отчёт; «отчёт устарел» - агент
молчит больше 3 минут; «с ошибками» - агент жив, но API telemt или mtg не ответил; «не в сети» -
агент молчит больше 10 минут или отчёта ещё не было.
Светлая и тёмная темы, русский и английский. Разработка и сборка - [internal/server/web/README.md](internal/server/web/README.md).

## Команды (CLI, доступно сейчас)

```sh
telemux probe --api http://127.0.0.1:9091        # проверить связь с telemt-API ноды
telemux role  --db "$DATABASE_URL"               # роль ноды: master/replica (из pg_is_in_recovery)
telemux fence --db "$DATABASE_URL" status        # PG-fence: включён ли read-only (аварийно: off)
telemux sync  --db "$DATABASE_URL" --api http://127.0.0.1:9091          # shadow: показать diff
telemux sync  --db "$DATABASE_URL" --api http://127.0.0.1:9091 --apply  # применить
telemux target --code pr2 --kind relay --resources telemt.users,tls.web --web-domains web2.example.com  # домены релея
telemux servers import --db "$DATABASE_URL" servers.json   # импорт серверов (массив, до 100)
telemux stack-apply --product telemt --sha256 <s> --version 3.5.17 --size <n>  # заменить бинарь (обычно запускает агент)
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
- [x] Веб-панель (React + antd, вшита в бинарь): обзор, пары, подключения, машины с карточкой машины, пользователи, дрейф, сертификаты, синк, API.
- [ ] Панель флота: «Пары», «Дрейф», журнал применений, раскатки - на живых данных мастера.
- [ ] Telegram-бот.

## Лицензия

[GPL-3.0](LICENSE). Часть кода веб-панели основана на [3x-ui](https://github.com/MHSanaei/3x-ui) (GPL-3.0); у заимствованных файлов сохраняется указание на источник.
