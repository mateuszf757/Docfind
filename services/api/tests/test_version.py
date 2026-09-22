"""Testy tożsamości builda.

Najważniejszy jest ostatni: brak version.json ma dawać jawne "unknown",
nie wyjątek i nie zmyśloną wersję.
"""

from __future__ import annotations

import json

import pytest
from fastapi.testclient import TestClient

from docfind_api import version as version_mod
from docfind_api.main import app


@pytest.fixture(autouse=True)
def _clear_cache():
    version_mod.build_info.cache_clear()
    yield
    version_mod.build_info.cache_clear()


@pytest.fixture
def client() -> TestClient:
    return TestClient(app)


def test_version_reads_build_file(tmp_path, monkeypatch, client):
    payload = {
        "version": "1.2.3",
        "commit": "0123456789abcdef0123456789abcdef01234567",
        "built_at": "2026-09-22T10:00:00Z",
    }
    build_file = tmp_path / "version.json"
    build_file.write_text(json.dumps(payload), encoding="utf-8")
    monkeypatch.setenv(version_mod.VERSION_FILE_ENV, str(build_file))

    assert client.get("/version").json() == payload


def test_version_is_unknown_without_build_file(tmp_path, monkeypatch, client):
    monkeypatch.setenv(version_mod.VERSION_FILE_ENV, str(tmp_path / "brak.json"))

    assert client.get("/version").json() == version_mod.UNKNOWN


def test_version_is_unknown_on_corrupt_build_file(tmp_path, monkeypatch, client):
    build_file = tmp_path / "version.json"
    build_file.write_text("{to nie jest json", encoding="utf-8")
    monkeypatch.setenv(version_mod.VERSION_FILE_ENV, str(build_file))

    assert client.get("/version").json() == version_mod.UNKNOWN


def test_partial_build_file_fills_missing_fields(tmp_path, monkeypatch, client):
    build_file = tmp_path / "version.json"
    build_file.write_text(json.dumps({"version": "9.9.9"}), encoding="utf-8")
    monkeypatch.setenv(version_mod.VERSION_FILE_ENV, str(build_file))

    body = client.get("/version").json()
    assert body["version"] == "9.9.9"
    assert body["commit"] == "unknown"
