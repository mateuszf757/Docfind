"""Dostęp do sekretów przez jedno wejście.

Każde miejsce potrzebujące sekretu woła get_secret(). Dziś stoi za tym
zmienna środowiskowa albo plik zamontowany przez Kubernetes; na Etapie 4
wejdzie External Secrets Operator z Vaultem i żadne miejsce wywołania
się nie zmieni. O to chodzi — inaczej zmiana źródła sekretów oznacza
przepisanie wszystkiego, co ich używa.

Wartość sekretu nigdy nie trafia do komunikatu błędu ani do logu.
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Final

SECRET_ENV_PREFIX: Final = "DOCFIND_SECRET_"
SECRETS_DIR_ENV: Final = "DOCFIND_SECRETS_DIR"
DEFAULT_SECRETS_DIR: Final = Path("/run/secrets")


class SecretNotFound(LookupError):
    """Sekret nie jest dostępny w żadnym ze źródeł."""


def _env_variable(name: str) -> str:
    return f"{SECRET_ENV_PREFIX}{name.upper()}"


def secrets_dir() -> Path:
    return Path(os.environ.get(SECRETS_DIR_ENV, DEFAULT_SECRETS_DIR))


def get_secret(name: str, *, default: str | None = None) -> str:
    """Zwraca sekret o podanej nazwie.

    Kolejność źródeł: zmienna środowiskowa, potem plik w katalogu sekretów.
    Zmienna wygrywa, bo jest jawnym nadpisaniem na czas diagnozy.

    Plik jest obcinany z końcowego znaku nowej linii — `echo hasło > plik`
    dokłada go po cichu i jest to klasyczna przyczyna nieudanego
    uwierzytelnienia, którą widać dopiero po porównaniu długości.
    """
    if not name:
        raise ValueError("nazwa sekretu nie może być pusta")

    from_env = os.environ.get(_env_variable(name))
    if from_env is not None:
        return from_env

    path = secrets_dir() / name
    try:
        return path.read_text(encoding="utf-8").removesuffix("\n")
    except FileNotFoundError:
        pass
    except OSError as exc:
        # Komunikat niesie ścieżkę i powód, nigdy zawartość.
        raise SecretNotFound(f"nie udało się odczytać sekretu '{name}' z {path}: {exc}") from exc

    if default is not None:
        return default

    raise SecretNotFound(
        f"brak sekretu '{name}': sprawdzono zmienną {_env_variable(name)} oraz plik {path}"
    )
