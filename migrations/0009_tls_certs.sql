-- telemux: сертификаты WEB-доменов релеев (ресурс tls.web, ADR 0004).
-- Выпускает и продлевает мастер (ACME DNS-01 через Cloudflare), агенты релеев
-- раскладывают их для nginx. Список доменов релея - targets.settings.web_domains.
-- Идемпотентна (IF NOT EXISTS).

BEGIN;

-- Сертификат домена. Строка появляется и при первой неудаче выпуска (без
-- сертификата, с last_error): по last_try_at цикл не повторяет выпуск чаще
-- раза в 6 часов, чтобы не упереться в лимиты Let's Encrypt.
CREATE TABLE IF NOT EXISTS tls_certs (
    domain      TEXT PRIMARY KEY,
    cert_pem    TEXT,                 -- полная цепочка (fullchain)
    key_pem     TEXT,                 -- закрытый ключ; наружу API не отдаёт
    not_after   TIMESTAMPTZ,
    issued_at   TIMESTAMPTZ,
    last_error  TEXT,                 -- последняя ошибка выпуска; NULL - последний выпуск удался
    last_try_at TIMESTAMPTZ,
    CHECK ((cert_pem IS NULL) = (key_pem IS NULL)),
    CHECK ((cert_pem IS NULL) = (not_after IS NULL))
);

-- Аккаунт ACME: свой на каждый каталог (продакшн, staging, pebble - разные аккаунты).
CREATE TABLE IF NOT EXISTS acme_account (
    directory    TEXT PRIMARY KEY,    -- URL каталога ACME
    email        TEXT NOT NULL,
    key_pem      TEXT NOT NULL,       -- ключ аккаунта (PKCS#8)
    registration JSONB,               -- ответ регистрации; NULL - ещё не зарегистрирован
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
