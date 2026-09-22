"""Testy endpointu wyszukiwania.

Na Etapie 0 sprawdzamy wyłącznie kontrakt: kształt odpowiedzi i walidację
wejścia. Treść wypełnia się na Etapie 6 i 7.
"""

from __future__ import annotations

from fastapi import status
from fastapi.testclient import TestClient

from tests.data import SAMPLE_QUERY


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
