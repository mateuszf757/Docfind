"""Wejście procesu: python -m docfind_api.

Konfiguracja jest walidowana zanim wstanie serwer. Dzięki temu błąd
konfiguracji kończy się czytelnym komunikatem i kodem wyjścia 1, a nie
serwerem, który wstał i przewraca się przy pierwszym żądaniu — wtedy
w logu widać skutek, a nie przyczynę.
"""

from __future__ import annotations

import sys

import uvicorn

from docfind_api.config import ConfigError, load_config
from docfind_api.main import create_app


def main() -> int:
    try:
        config = load_config()
    except ConfigError as exc:
        print(f"BŁĄD KONFIGURACJI\n{exc}", file=sys.stderr)
        return 1

    uvicorn.run(
        create_app(config),
        host=config.service.host,
        port=config.service.port,
        log_level=config.service.log_level.value,
        # Krótki limit na dokończenie żądań: przy wymianie poda kubelet i tak
        # czeka tylko terminationGracePeriodSeconds, a zwlekanie wydłuża
        # rolling update o iloczyn tego czasu i liczby replik.
        timeout_graceful_shutdown=config.service.shutdown_grace_seconds,
        timeout_keep_alive=config.service.keep_alive_seconds,
        access_log=False,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
