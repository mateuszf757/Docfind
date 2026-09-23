"""Wspólne wyposażenie testów.

Dane testowe siedzą w tests/data.py — asercje mają mówić o zachowaniu,
nie o literałach.
"""

from __future__ import annotations

import json
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import Any

import pytest
from fastapi.testclient import TestClient

from docfind_api import config as config_module
from docfind_api import secrets as secrets_module
from docfind_api import version as version_module
from docfind_api.main import app

WriteRawVersionFile = Callable[[str], Path]
WriteBuildInfo = Callable[[dict[str, Any]], Path]
WriteConfigFile = Callable[[str], Path]


@pytest.fixture(autouse=True)
def _isolated_build_info_cache() -> Iterator[None]:
    """Izoluje cache build_info().

    Funkcja jest cache'owana na czas życia procesu, więc bez czyszczenia
    drugi test w pliku zobaczyłby tożsamość podstawioną przez pierwszy.
    """
    version_module.build_info.cache_clear()
    yield
    version_module.build_info.cache_clear()


@pytest.fixture
def client() -> TestClient:
    return TestClient(app)


@pytest.fixture
def given_raw_version_file(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> WriteRawVersionFile:
    """Podstawia surową treść version.json widzianą przez aplikację."""

    def _write(content: str) -> Path:
        path = tmp_path / "version.json"
        path.write_text(content, encoding="utf-8")
        monkeypatch.setenv(version_module.VERSION_FILE_ENV, str(path))
        return path

    return _write


@pytest.fixture
def given_build_info(given_raw_version_file: WriteRawVersionFile) -> WriteBuildInfo:
    """Wariant dla poprawnego JSON-a — przyjmuje słownik, nie tekst."""

    def _write(payload: dict[str, Any]) -> Path:
        return given_raw_version_file(json.dumps(payload))

    return _write


@pytest.fixture
def given_no_version_file(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Wskazuje aplikację na nieistniejący plik — kod spoza obrazu."""
    path = tmp_path / "nie-istnieje.json"
    monkeypatch.setenv(version_module.VERSION_FILE_ENV, str(path))
    return path


@pytest.fixture(autouse=True)
def _isolated_config_cache() -> Iterator[None]:
    """Izoluje cache get_config() — jak przy build_info()."""
    config_module.get_config.cache_clear()
    yield
    config_module.get_config.cache_clear()


@pytest.fixture
def given_config_file(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> WriteConfigFile:
    """Podstawia treść app.yml widzianą przez aplikację."""

    def _write(content: str) -> Path:
        path = tmp_path / "app.yml"
        path.write_text(content, encoding="utf-8")
        monkeypatch.setenv(config_module.CONFIG_FILE_ENV, str(path))
        return path

    return _write


@pytest.fixture
def given_secrets_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Katalog udający montowanie sekretów przez Kubernetes."""
    directory = tmp_path / "secrets"
    directory.mkdir()
    monkeypatch.setenv(secrets_module.SECRETS_DIR_ENV, str(directory))
    return directory
