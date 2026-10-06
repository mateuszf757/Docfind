"""Testy endpointu wyszukiwania.

Na Etapie 0 sprawdzamy wyłącznie kontrakt: kształt odpowiedzi i walidację
wejścia. Treść wypełnia się na Etapie 6 i 7.
"""

from __future__ import annotations

import time

import yaml
from fastapi import status
from fastapi.testclient import TestClient

from docfind_api.config import AppConfig
from docfind_api.main import create_app
from tests.data import MINIMAL_CONFIG_YAML, SAMPLE_QUERY


def test_returns_empty_result_set(client: TestClient) -> None:
    response = client.get("/search", params={"q": SAMPLE_QUERY})

    assert response.status_code == status.HTTP_200_OK
    assert response.json() == {"query": SAMPLE_QUERY, "fragments": [], "answer": None}


def test_rejects_empty_query(client: TestClient) -> None:
    response = client.get("/search", params={"q": ""})

    assert response.status_code == status.HTTP_422_UNPROCESSABLE_CONTENT


def test_rejects_missing_query(client: TestClient) -> None:
    response = client.get("/search")

    assert response.status_code == status.HTTP_422_UNPROCESSABLE_CONTENT


def test_stub_delay_simulates_model_latency() -> None:
    """Zaślepka z opóźnieniem trzyma żądanie w locie co najmniej tyle, ile każe
    konfiguracja — na tym opiera się bramka zamykania przy SIGTERM.

    Asercja tylko od dołu: sen trwa co najmniej zadany czas, górna granica
    zależałaby od obciążenia maszyny.
    """
    raw = yaml.safe_load(MINIMAL_CONFIG_YAML)
    raw["llm"] = {"stub_delay_ms": 50}
    config = AppConfig.model_validate(raw)

    with TestClient(create_app(config)) as client:
        started = time.monotonic()
        response = client.get("/search", params={"q": SAMPLE_QUERY})
        elapsed = time.monotonic() - started

    assert response.status_code == status.HTTP_200_OK
    assert elapsed >= 0.05
