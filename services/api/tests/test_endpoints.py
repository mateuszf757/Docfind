from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from docfind_api.main import app


@pytest.fixture
def client() -> TestClient:
    return TestClient(app)


def test_healthz(client):
    assert client.get("/healthz").status_code == 200


def test_readyz(client):
    assert client.get("/readyz").status_code == 200


def test_search_returns_stub(client):
    body = client.get("/search", params={"q": "kubernetes"}).json()
    assert body["query"] == "kubernetes"
    assert body["fragments"] == []


def test_search_rejects_empty_query(client):
    assert client.get("/search", params={"q": ""}).status_code == 422
