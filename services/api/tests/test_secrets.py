"""Testy dostępu do sekretów.

Jedno wejście dla wszystkich źródeł, żeby wejście Vaulta na Etapie 4 nie
wymagało dotykania miejsc wywołania.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from docfind_api.secrets import SECRET_ENV_PREFIX, SecretNotFound, get_secret
from tests.data import SECRET_NAME, SECRET_VALUE


def test_reads_from_environment(monkeypatch: pytest.MonkeyPatch, given_secrets_dir: Path) -> None:
    monkeypatch.setenv(f"{SECRET_ENV_PREFIX}{SECRET_NAME.upper()}", SECRET_VALUE)

    assert get_secret(SECRET_NAME) == SECRET_VALUE


def test_reads_from_mounted_file(given_secrets_dir: Path) -> None:
    (given_secrets_dir / SECRET_NAME).write_text(SECRET_VALUE, encoding="utf-8")

    assert get_secret(SECRET_NAME) == SECRET_VALUE


def test_strips_trailing_newline_from_file(given_secrets_dir: Path) -> None:
    """`echo hasło > plik` dokłada znak nowej linii po cichu.

    To klasyczna przyczyna nieudanego uwierzytelnienia, którą widać dopiero
    po porównaniu długości wartości.
    """
    (given_secrets_dir / SECRET_NAME).write_text(f"{SECRET_VALUE}\n", encoding="utf-8")

    assert get_secret(SECRET_NAME) == SECRET_VALUE


def test_environment_wins_over_file(
    monkeypatch: pytest.MonkeyPatch, given_secrets_dir: Path
) -> None:
    """Zmienna jest jawnym nadpisaniem na czas diagnozy, więc ma pierwszeństwo."""
    (given_secrets_dir / SECRET_NAME).write_text("z-pliku", encoding="utf-8")
    monkeypatch.setenv(f"{SECRET_ENV_PREFIX}{SECRET_NAME.upper()}", "ze-zmiennej")

    assert get_secret(SECRET_NAME) == "ze-zmiennej"


def test_falls_back_to_default(given_secrets_dir: Path) -> None:
    assert get_secret(SECRET_NAME, default="zapasowe") == "zapasowe"


def test_missing_secret_names_both_sources(given_secrets_dir: Path) -> None:
    with pytest.raises(SecretNotFound) as caught:
        get_secret(SECRET_NAME)

    message = str(caught.value)
    assert f"{SECRET_ENV_PREFIX}{SECRET_NAME.upper()}" in message
    assert SECRET_NAME in message


def test_error_never_leaks_the_value(given_secrets_dir: Path) -> None:
    """Komunikat błędu niesie ścieżkę i powód, nigdy zawartość sekretu."""
    path = given_secrets_dir / SECRET_NAME
    path.mkdir()  # katalog zamiast pliku — odczyt zawiedzie na IsADirectoryError

    with pytest.raises(SecretNotFound) as caught:
        get_secret(SECRET_NAME)

    assert SECRET_VALUE not in str(caught.value)


def test_empty_name_is_rejected() -> None:
    with pytest.raises(ValueError, match="nie może być pusta"):
        get_secret("")
