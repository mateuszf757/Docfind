"""Testy tokenu dostępu do /metrics.

Najważniejszy jest ten o rotacji: ta sama instancja aplikacji, bez
restartu, przyjmuje nowy token i odrzuca stary, gdy zmienia się plik
sekretu — tak, jak Kubernetes podmienia pliki Secreta w wolumenie. Odczyt
raz przy starcie przeszedłby wszystkie inne testy.
"""

from __future__ import annotations

from collections.abc import Iterator
from pathlib import Path

import pytest
import yaml
from fastapi import status
from fastapi.testclient import TestClient

from docfind_api.__main__ import main
from docfind_api.config import AppConfig
from docfind_api.main import create_app
from docfind_api.tokens import authorized, bearer
from tests.conftest import WriteConfigFile
from tests.data import (
    CONFIG_WITH_METRICS_TOKEN,
    METRICS_TOKEN_NEW,
    METRICS_TOKEN_OLD,
    METRICS_TOKEN_SECRET,
    METRICS_TOKEN_WRONG,
)


def write_tokens(secrets_dir: Path, *tokens: str) -> None:
    (secrets_dir / METRICS_TOKEN_SECRET).write_text("\n".join(tokens) + "\n", encoding="utf-8")


def auth(token: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token}"}


@pytest.fixture
def protected_client(given_secrets_dir: Path) -> Iterator[TestClient]:
    write_tokens(given_secrets_dir, METRICS_TOKEN_OLD)
    with TestClient(create_app(AppConfig.model_validate(CONFIG_WITH_METRICS_TOKEN))) as client:
        yield client


def test_metrics_require_token(protected_client: TestClient) -> None:
    missing = protected_client.get("/metrics")
    assert missing.status_code == status.HTTP_401_UNAUTHORIZED
    assert missing.headers["www-authenticate"] == "Bearer"
    assert (
        protected_client.get("/metrics", headers=auth(METRICS_TOKEN_WRONG)).status_code
        == status.HTTP_401_UNAUTHORIZED
    )
    assert (
        protected_client.get("/metrics", headers=auth(METRICS_TOKEN_OLD)).status_code
        == status.HTTP_200_OK
    )


def test_rotation_reaches_running_process(
    protected_client: TestClient, given_secrets_dir: Path
) -> None:
    # Faza 1: nowy token obok starego — obaj klienci przechodzą.
    write_tokens(given_secrets_dir, METRICS_TOKEN_OLD, METRICS_TOKEN_NEW)
    assert (
        protected_client.get("/metrics", headers=auth(METRICS_TOKEN_NEW)).status_code
        == status.HTTP_200_OK
    )
    assert (
        protected_client.get("/metrics", headers=auth(METRICS_TOKEN_OLD)).status_code
        == status.HTTP_200_OK
    )

    # Faza 2: bez starego — stary odrzucony, nowy dalej działa.
    write_tokens(given_secrets_dir, METRICS_TOKEN_NEW)
    assert (
        protected_client.get("/metrics", headers=auth(METRICS_TOKEN_OLD)).status_code
        == status.HTTP_401_UNAUTHORIZED
    )
    assert (
        protected_client.get("/metrics", headers=auth(METRICS_TOKEN_NEW)).status_code
        == status.HTTP_200_OK
    )


def test_unreadable_secret_closes_metrics(
    protected_client: TestClient, given_secrets_dir: Path
) -> None:
    # Brak sekretu w trakcie działania to metryki niedostępne, a nie otwarte.
    (given_secrets_dir / METRICS_TOKEN_SECRET).unlink()
    response = protected_client.get("/metrics", headers=auth(METRICS_TOKEN_OLD))
    assert response.status_code == status.HTTP_503_SERVICE_UNAVAILABLE
    assert METRICS_TOKEN_OLD not in response.text


def test_other_endpoints_stay_open(protected_client: TestClient) -> None:
    assert protected_client.get("/healthz").status_code == status.HTTP_200_OK


@pytest.mark.parametrize(
    ("header", "expected"),
    [
        (f"Bearer {METRICS_TOKEN_OLD}", METRICS_TOKEN_OLD),
        (f"bearer {METRICS_TOKEN_OLD} ", METRICS_TOKEN_OLD),
        (f"Basic {METRICS_TOKEN_OLD}", None),
        ("Bearer ", None),
        (None, None),
    ],
)
def test_bearer_header(header: str | None, expected: str | None) -> None:
    assert bearer(header) == expected


def test_authorized_checks_every_token() -> None:
    assert authorized(METRICS_TOKEN_NEW, (METRICS_TOKEN_OLD, METRICS_TOKEN_NEW))
    assert not authorized(METRICS_TOKEN_NEW[:-1], (METRICS_TOKEN_OLD, METRICS_TOKEN_NEW))


def test_start_refused_without_secret(
    given_config_file: WriteConfigFile, given_secrets_dir: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    given_config_file(yaml.safe_dump(CONFIG_WITH_METRICS_TOKEN))
    assert main() == 1
    assert "BŁĄD SEKRETÓW" in capsys.readouterr().err


def test_start_refused_with_empty_secret(
    given_config_file: WriteConfigFile, given_secrets_dir: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    write_tokens(given_secrets_dir, "", "  ")
    given_config_file(yaml.safe_dump(CONFIG_WITH_METRICS_TOKEN))
    assert main() == 1
    assert "nie zawiera żadnego tokenu" in capsys.readouterr().err
