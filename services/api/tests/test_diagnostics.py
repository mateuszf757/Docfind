"""Testy endpointów diagnostycznych."""

from __future__ import annotations

from fastapi import status
from fastapi.testclient import TestClient

from docfind_api.models import ReadinessCheck
from docfind_api.routers import diagnostics


def test_healthz_reports_alive(client: TestClient) -> None:
    response = client.get("/healthz")

    assert response.status_code == status.HTTP_200_OK
    assert response.json() == {"status": "ok"}


def test_readyz_is_ready_without_dependencies(client: TestClient) -> None:
    response = client.get("/readyz")

    assert response.status_code == status.HTTP_200_OK
    assert response.json() == {"ready": True, "checks": []}


def test_readyz_returns_503_when_a_dependency_is_unhealthy(client: TestClient, monkeypatch) -> None:
    """Niegotowość musi być widoczna w kodzie odpowiedzi.

    Sam kod 200 z polem ready=false nie wycina poda z endpointów Service,
    bo kubelet patrzy na status HTTP, nie na treść.
    """
    failing = ReadinessCheck(name="elasticsearch", healthy=False, detail="brak aliasu docfind")
    monkeypatch.setattr(diagnostics, "_readiness_checks", lambda: [failing])

    response = client.get("/readyz")

    assert response.status_code == status.HTTP_503_SERVICE_UNAVAILABLE
    assert response.json()["ready"] is False
    assert response.json()["checks"] == [failing.model_dump()]
