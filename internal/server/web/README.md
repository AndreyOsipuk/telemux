# Веб-панель telemux

React 19 + TypeScript, antd 6, @tanstack/react-query, react-router, i18next (ru/en), Vite.
Основа - панель форка 3x-ui (GPL-3.0); в заимствованных файлах первой строкой указан источник.

Собранная панель (`dist/`) **коммитится** и вшивается в Go-бинарь через `embed`
(см. [../web.go](../web.go)), поэтому для сборки демона Node не нужен.

## Команды

```sh
cd internal/server/web
npm ci            # зависимости (Node 24+)
npm run dev       # dev-сервер Vite, /api проксируется на http://localhost:8080
PREVIEW_SSH=<алиас> npm run dev:preview  # то же на живых данных превью (туннель, см. ниже)
npm test          # vitest + React Testing Library
npm run typecheck # tsc --noEmit
npm run build     # tsc + сборка в dist/ - её и коммитим вместе с исходниками
npm run gen:api   # типы из ../../../api/openapi.yaml -> src/api/generated/openapi.ts (коммитится)
```

Адрес демона для dev-сервера меняется переменной `TELEMUX_DEV_BACKEND`. Разработка на живых
данных без риска для прода - через превью: [scripts/preview/README.md](../../../scripts/preview/README.md).

## Устройство

| Каталог | Что там |
|---|---|
| `src/api/http.ts` | `request()`: ошибки API -> `ApiError {status, code, message, masterUrl}`; 401 -> событие «нужен вход» |
| `src/api/errors.ts` | понятный текст ошибки: 409 `not_master` -> «эта машина не мастер» с адресом мастера, сеть, 503, 5xx |
| `src/api/client.ts` | ручки, которыми пользуется панель; флот, серверы, тревоги, версии - `/api/v1`, постранично |
| `src/api/types.ts` | типы: из спеки (`generated/openapi.ts`) и временные для старых ручек, которых в спеке ещё нет |
| `src/api/queries.ts` | хуки react-query; экраны флота обновляются раз в 30 секунд, подключения - раз в 15, идущее обновление версий - раз в 5 |
| `src/pages/overview/` | «Обзор» - главная (`/`): плитки итогов и проблемы машин, последние тревоги, ошибки журнала |
| `src/pages/servers/` | «Серверы»: список, форма, вкладка «Сборка конфига» (diff и источник конфигов) |
| `src/pages/map/` | «Карта»: релеи, серверы по номеру, машины за рубежом; наведение подсвечивает путь |
| `src/pages/nodes/` | «Машины»: таблица флота с действиями, «Версии и обновления» (telemt, mtg-multi), узлы кластера |
| `src/pages/connections/` | «Подключения»: сводка - строка на машину, пользователи онлайн с фильтрами |
| `src/pages/events/` | «События»: лента тревог (`/api/v1/events`) |
| `src/pages/users/` | «Пользователи» и карточка пользователя (срок, подключения) |
| `src/components/MachineDrawer.tsx` | карточка машины: состояние, нагрузка (графики), связь, ресурсы и ревизии, подключения, ошибки журнала, история применений, сертификаты |
| `src/utils/machineUsage.ts` | роль машины по серверам: серые счётчики у выходов «только через релей» и выведенных машин |
| `src/components/MachineCells.tsx` | runtime-колонки машины (статус, нагрузка, аптайм, подключения, версии) для «Обзора» и «Машин» |
| `src/components/ConnectionsTable.tsx`, `CertsTable.tsx`, `LogErrorsTable.tsx` | таблицы с серверной пагинацией - на своих экранах и во вкладках карточки машины |
| `src/components/JoinCommand.tsx` | одноразовая команда установки агента (копирование, срок, файрвол) и действие «Команда установки» у машины (`POST /api/v1/targets/{code}/join`) |
| `src/pages/nodes/AddMachineModal.tsx` | «Добавить машину» (`POST /api/v1/targets`): код, роль, ресурсы, для выхода - релей, IPv4 и socks-вход на релее |
| `src/hooks/usePaging.ts` | серверная пагинация `{data, paging}` для таблиц |
| `src/auth/AuthGate.tsx` | вход по сессии (`/api/me`, `/api/login`, `/api/logout`) |
| `src/layouts/` | оболочка страниц и сайдбар |
| `src/pages/*` | экраны, грузятся лениво |
| `src/i18n/` | словари ru/en; тест `i18n-keys` проверяет, что ключи совпадают |
| `src/test/` | тесты |

Раздача статики (`../web.go`): `assets/*` с хешем в имени кэшируются навсегда, `index.html` -
`no-cache`; неизвестный путь вне `/api/` отдаёт `index.html` (клиентский роутинг), а
несуществующий `assets/*` - 404. Вкладка, открытая до обновления бинаря, на такой 404
перезагружается один раз (`src/chunkRecovery.ts`).

Генератор (`openapi-typescript`) запускается через `npx` с закреплёнными версиями: ему нужен
TypeScript 5 с JS-API, а сам проект проверяется TypeScript 7.
