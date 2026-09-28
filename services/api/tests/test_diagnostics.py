"""Testy endpointów diagnostycznych."""

from __future__ import annotations

import threading

import anyio
import anyio.from_thread
import anyio.to_thread
import httpx2
import pytest
from fastapi import FastAPI, status
from fastapi.testclient import TestClient

from docfind_api.config import AppConfig, ServiceConfig
from docfind_api.main import create_app
from docfind_api.models import ReadinessCheck
from docfind_api.routers import diagnostics

DIAGNOSTIC_ROUTES = ["/healthz", "/readyz", "/version", "/metrics"]


def test_healthz_reports_alive(client: TestClient) -> None:
    response = client.get("/healthz")

    assert response.status_code == status.HTTP_200_OK
    assert response.json() == {"status": "ok"}


def test_readyz_is_ready_without_dependencies(client: TestClient) -> None:
    response = client.get("/readyz")

    assert response.status_code == status.HTTP_200_OK
    assert response.json() == {"ready": True, "checks": []}


def test_readyz_returns_503_when_a_dependency_is_unhealthy(
    client: TestClient, monkeypatch: pytest.MonkeyPatch
) -> None:
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


@pytest.mark.anyio
@pytest.mark.parametrize("route", DIAGNOSTIC_ROUTES)
async def test_diagnostics_answer_when_thread_pool_is_exhausted(app: FastAPI, route: str) -> None:
    """Sonda nie może czekać w kolejce do puli wątków.

    Pula anyio dostaje jedno miejsce, a zajmuje je blokujące wywołanie —
    tak jak zajęłyby ją wolne zapytania do Elasticsearcha. Handler `def`
    czekałby na wolny wątek do końca limitu i kubelet uznałby pod za martwy;
    `async def` odpowiada od razu. Zmiana /healthz na `def` wywraca ten test.
    """
    anyio.to_thread.current_default_thread_limiter().total_tokens = 1
    occupied = anyio.Event()
    release = threading.Event()

    def occupy_the_only_thread() -> None:
        anyio.from_thread.run_sync(occupied.set)
        release.wait()

    transport = httpx2.ASGITransport(app=app)
    async with (
        httpx2.AsyncClient(transport=transport, base_url="http://docfind") as client,
        anyio.create_task_group() as tasks,
    ):
        tasks.start_soon(anyio.to_thread.run_sync, occupy_the_only_thread)
        try:
            with anyio.fail_after(2):
                await occupied.wait()
            with anyio.fail_after(2):
                response = await client.get(route)
        finally:
            release.set()

    assert response.status_code == status.HTTP_200_OK


def test_docs_are_disabled_by_default(client: TestClient) -> None:
    """Swagger UI ładuje kod z cdn.jsdelivr.net — nie serwujemy go bez decyzji."""
    assert client.get("/docs").status_code == status.HTTP_404_NOT_FOUND
    assert client.get("/openapi.json").status_code == status.HTTP_200_OK


def test_docs_can_be_enabled(app_config: AppConfig) -> None:
    config = app_config.model_copy(update={"service": ServiceConfig(docs=True)})

    with TestClient(create_app(config)) as client:
        assert client.get("/docs").status_code == status.HTTP_200_OK
