-- Генерирует telemt users.toml из боевой БД бота (tgbots), воспроизводя
-- apps/mtproxy/src/users-toml-builder.ts:buildUserKey один-в-один:
--   slug = lower(username) без не-буквоцифр, ≤16 → "<slug>_<sub.id>"
--   иначе "<source>_<source_id>_<sub.id>"
-- Фильтр как в generateUsersConfig: подписка active + сервер active + secret не пуст.
-- Выводит секции [access.users] / [access.user_max_tcp_conns] / [access.user_expirations]
-- для последующего `telemux import-toml`.
WITH base AS (
  SELECT
    s.id,
    s.secret,
    CASE
      WHEN left(regexp_replace(lower(coalesce(u.username, '')), '[^a-z0-9]+', '', 'g'), 16) <> ''
        THEN left(regexp_replace(lower(coalesce(u.username, '')), '[^a-z0-9]+', '', 'g'), 16) || '_' || s.id
      ELSE s.source || '_' || s.source_id || '_' || s.id
    END AS key,
    to_char(s.expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS exp
  FROM subscriptions s
  JOIN servers srv ON srv.id = s.server_id AND srv.is_active = true
  LEFT JOIN user_identities ui ON ui.provider = s.source AND ui.external_id::text = s.source_id::text
  LEFT JOIN users u ON u.id = ui.user_id
  WHERE s.is_active = true AND s.secret IS NOT NULL AND s.secret <> ''
)
SELECT
  '[access.users]' || chr(10)
  || string_agg(key || ' = "' || secret || '"', chr(10)) || chr(10) || chr(10)
  || '[access.user_max_tcp_conns]' || chr(10)
  || string_agg(key || ' = 25', chr(10)) || chr(10) || chr(10)
  || '[access.user_expirations]' || chr(10)
  || string_agg(key || ' = "' || exp || '"', chr(10))
FROM base;
