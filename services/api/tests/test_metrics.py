"""Testy metryk.

Najważniejszy jest ten o kardynalności: etykieta trasy musi być szablonem,
a trafienia nieistniejących adresów nie mogą tworzyć szeregów czasowych.
Skan katalogów po nieuważnie napisanym middleware potrafi przewrócić
Prometheusa na pamięci.

Każdy test ma własną aplikację, a więc własny rejestr — liczniki startują
od zera i asercje sprawdzają wartości, nie przyrosty.
"""

from __future__ import annotations

import sys

import pytest
from fastapi import FastAPI, status
from fastapi.testclient import TestClient

from docfind_api.config import AppConfig
from docfind_api.main import create_app
from docfind_api.metrics import HttpMetrics
from tests.data import SAMPLE_QUERY

REQUEST_METRIC = "docfind_http_requests_total"


def metrics_of(app: FastAPI) -> HttpMetrics:
    metrics: HttpMetrics = app.state.metrics
    return metrics


def requests_counted(app: FastAPI, route: str, method: str = "GET", code: str = "200") -> float:
    value = metrics_of(app).registry.get_sample_value(
        REQUEST_METRIC, {"method": method, "route": route, "status": code}
    )
    return value or 0.0


def test_metrics_endpoint_exposes_prometheus_format(client: TestClient) -> None:
    response = client.get("/metrics")

    assert response.status_code == status.HTTP_200_OK
    assert response.headers["content-type"].startswith("text/plain")


@pytest.mark.skipif(sys.platform != "linux", reason="ProcessCollector czyta /proc")
def test_process_metrics_survive_own_registry(client: TestClient) -> None:
    """Własny rejestr nie ma kolektorów procesu z urzędu.

    Bez ich jawnej rejestracji /metrics traci pamięć i CPU procesu — a to
    pierwsze, czego się szuka przy OOMKilled albo nagłym wzroście opóźnień.
    """
    exported = client.get("/metrics").text

    assert "process_resident_memory_bytes" in exported
    assert "python_gc_objects_collected_total" in exported


def test_search_requests_are_counted(app: FastAPI, client: TestClient) -> None:
    client.get("/search", params={"q": SAMPLE_QUERY})
    client.get("/search", params={"q": SAMPLE_QUERY})

    assert requests_counted(app, "/search") == 2


def test_latency_is_observed_for_search(app: FastAPI, client: TestClient) -> None:
    client.get("/search", params={"q": SAMPLE_QUERY})

    observed = metrics_of(app).registry.get_sample_value(
        "docfind_http_request_duration_seconds_count", {"method": "GET", "route": "/search"}
    )
    assert observed == 1


@pytest.mark.parametrize("route", ["/healthz", "/readyz", "/version", "/metrics"])
def test_diagnostic_routes_are_not_counted(app: FastAPI, client: TestClient, route: str) -> None:
    """Sondy kubeleta pukają co kilka sekund i zdominowałyby histogram.

    p95 liczone razem z /healthz nie mówi nic o czasie wyszukiwania.
    """
    client.get(route)

    assert requests_counted(app, route) == 0


def test_unmatched_paths_do_not_create_series(client: TestClient) -> None:
    """Każdy zgadywany adres tworzyłby własny szereg czasowy."""
    client.get("/nie-ma-takiej-trasy")
    client.get("/tez-nie-ma")

    exported = client.get("/metrics").text

    assert "/nie-ma-takiej-trasy" not in exported
    assert "/tez-nie-ma" not in exported


def test_route_label_is_a_template_not_a_path(client: TestClient) -> None:
    """Etykieta to '/search', nie '/search?q=...' ani ścieżka z parametrem."""
    client.get("/search", params={"q": "pierwsze"})
    client.get("/search", params={"q": "drugie"})

    exported = client.get("/metrics").text

    assert 'route="/search"' in exported
    assert "pierwsze" not in exported
    assert "drugie" not in exported


def test_instances_do_not_share_counters(app_config: AppConfig) -> None:
    """Dwie aplikacje w jednym procesie mają osobne liczniki.

    Z globalnym REGISTRY druga instancja dopisywałaby się do liczników
    pierwszej, a testy musiały porównywać przyrosty.
    """
    first, second = create_app(app_config), create_app(app_config)

    with TestClient(first) as client:
        client.get("/search", params={"q": SAMPLE_QUERY})

    assert requests_counted(first, "/search") == 1
    assert requests_counted(second, "/search") == 0
