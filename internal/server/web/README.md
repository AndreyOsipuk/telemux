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
npm test          # vitest + React Testing Library
npm run typecheck # tsc --noEmit
npm run build     # tsc + сборка в dist/ - её и коммитим вместе с исходниками
npm run gen:api   # типы из ../../../api/openapi.yaml -> src/api/generated/openapi.ts (коммитится)
```

Адрес демона для dev-сервера меняется переменной `TELEMUX_DEV_BACKEND`.

## Устройство

| Каталог | Что там |
|---|---|
| `src/api/http.ts` | `request()`: ошибки API -> `ApiError {status, code, message, masterUrl}`; 401 -> событие «нужен вход» |
| `src/api/errors.ts` | понятный текст ошибки: 409 `not_master` -> «эта машина не мастер» с адресом мастера, сеть, 503, 5xx |
| `src/api/client.ts` | ручки, которыми пользуется панель; флот (машины, пары, дрейф, журнал применений) - `/api/v1`, постранично |
| `src/api/types.ts` | типы: из спеки (`generated/openapi.ts`) и временные для старых ручек, которых в спеке ещё нет |
| `src/api/queries.ts` | хуки react-query; экраны флота (Обзор, Пары, Дрейф, Машины, Сертификаты) обновляются раз в 30 секунд, подключения - раз в 15 |
| `src/pages/overview/` | «Обзор» - главная (`/`): плитки итогов и машины компактной таблицей (`/api/v1/overview`) |
| `src/pages/connections/` | «Подключения»: сводка по машинам и продуктам, пользователи онлайн с фильтрами |
| `src/pages/certs/` | «Сертификаты» (`/api/v1/certs`) |
| `src/components/MachineDrawer.tsx` | карточка машины: состояние, ресурсы и ревизии, подключения, ошибки журнала, история применений, сертификаты |
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
