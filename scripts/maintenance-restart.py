#!/usr/bin/env python3
"""Окно обслуживания telemux вокруг перезапуска и проверки готовности."""

import argparse
import ipaddress
import json
import re
import subprocess
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

TARGET_CODE = re.compile(r"[a-z][a-z0-9-]{1,31}")
MAX_SECONDS = 900
MAX_TARGETS = 100


class MasterError(Exception):
    """Причина ошибки без ответа сервера и учётных данных."""

    def __init__(self, message, status=None):
        super().__init__(message)
        self.status = status


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Master:
    def __init__(self, origin, token):
        self.origin = origin
        self.token = token
        self.opener = build_opener(ProxyHandler({}), NoRedirect())

    def set_window(self, target, until, expected):
        payload = {
            "settings": {"maintenance_until": until},
            "expected_maintenance_until": expected,
        }
        req = Request(
            f"{self.origin}/api/v1/targets/{target}",
            data=json.dumps(payload).encode(),
            headers={"Authorization": f"Bearer {self.token}", "Content-Type": "application/json"},
            method="PUT",
        )
        try:
            with self.opener.open(req, timeout=8) as response:
                if response.status != 200:
                    raise MasterError(f"HTTP {response.status}", response.status)
        except HTTPError as err:
            err.close()
            raise MasterError(f"HTTP {err.code}", err.code) from None
        except (URLError, OSError):
            raise MasterError("ответ мастера не получен") from None


def master_origin(raw):
    try:
        url = urlsplit(raw)
        port = url.port
    except ValueError:
        raise ValueError("неверный адрес мастера") from None
    if (
        url.scheme not in ("http", "https") or not url.hostname or url.username is not None
        or url.password is not None or url.path not in ("", "/") or re.search(r"[\s\\?#]", raw)
        or port == 0
    ):
        raise ValueError("нужен адрес мастера без пути и учётных данных")
    if url.scheme == "http":
        try:
            loopback = ipaddress.ip_address(url.hostname).is_loopback
        except ValueError:
            loopback = url.hostname == "localhost"
        if not loopback:
            raise ValueError("для удалённого мастера нужен HTTPS")
    return raw.rstrip("/")


def read_token(path):
    try:
        with Path(path).open(encoding="utf-8") as file:
            token = file.read(4097).strip()
    except (OSError, UnicodeError):
        raise ValueError("не удалось прочитать файл токена") from None
    if not token or len(token) > 4096 or re.search(r"[^!-~]", token):
        raise ValueError("неверное содержимое файла токена")
    return token


def close_windows(master, targets, until, ignore_conflict=False):
    failed = False
    for target in reversed(targets):
        try:
            master.set_window(target, "", until)
        except MasterError as err:
            if ignore_conflict and err.status == 409:
                continue
            failed = True
            print(f"{target}: окно не закрыто ({err}), срок ограничен", file=sys.stderr)
    return not failed


def restart(master, targets, seconds, command):
    deadline = datetime.now(timezone.utc) + timedelta(seconds=seconds)
    until = deadline.isoformat(timespec="microseconds").replace("+00:00", "Z")
    attempted = []
    try:
        for target in targets:
            # При потерянном HTTP-ответе запись могла сохраниться. CAS при снятии
            # окна защитит состояние, если окно на самом деле принадлежит другому.
            attempted.append(target)
            master.set_window(target, until, "")
    except MasterError as err:
        print(f"{attempted[-1]}: окно не открыто ({err}), отменяю перезапуск", file=sys.stderr)
        close_windows(master, attempted, until, ignore_conflict=True)
        return 1
    except KeyboardInterrupt:
        close_windows(master, attempted, until, ignore_conflict=True)
        return 130

    if datetime.now(timezone.utc) >= deadline:
        print("Срок окна истёк до запуска команды, перезапуск отменён", file=sys.stderr)
        close_windows(master, attempted, until, ignore_conflict=True)
        return 1
    print(f"Окно обслуживания до {until}: {', '.join(targets)}", flush=True)
    try:
        code = subprocess.run(command, check=False).returncode
    except OSError:
        code = 127
    except KeyboardInterrupt:
        code = 130
    if code:
        print(f"Перезапуск или проверка завершились с ошибкой, окно действует до {until}", file=sys.stderr)
        return code if code > 0 else 128 - code

    if not close_windows(master, targets, until):
        return 1
    print("Перезапуск подтверждён, окна закрыты")
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--master", required=True, help="URL API мастера")
    parser.add_argument("--token-file", required=True, help="путь к файлу с токеном API")
    parser.add_argument("--target", required=True, action="append", help="код машины, повторяется для нескольких машин")
    parser.add_argument("--seconds", type=int, default=180, help=f"длительность окна, 1–{MAX_SECONDS} секунд")
    parser.add_argument("command", nargs=argparse.REMAINDER, help="команда перезапуска и проверки после --")
    args = parser.parse_args(argv)
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    targets = list(dict.fromkeys(args.target))
    if not command:
        parser.error("задайте команду перезапуска и проверки после --")
    if not 1 <= args.seconds <= MAX_SECONDS:
        parser.error(f"длительность должна быть от 1 до {MAX_SECONDS} секунд")
    if len(targets) > MAX_TARGETS or any(not TARGET_CODE.fullmatch(t) for t in targets):
        parser.error("слишком много целей или некорректный код машины")
    try:
        origin = master_origin(args.master)
        token = read_token(args.token_file)
    except ValueError as err:
        parser.error(str(err))
    return restart(Master(origin, token), targets, args.seconds, command)


if __name__ == "__main__":
    sys.exit(main())
