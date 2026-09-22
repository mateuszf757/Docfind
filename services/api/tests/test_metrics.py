"""Testy metryk.

Najważniejszy jest ten o kardynalności: etykieta trasy musi być szablonem,
a trafienia nieistniejących adresów nie mogą tworzyć szeregów czasowych.
Skan katalogów po nieuważnie napisanym middleware potrafi przewrócić
Prometheusa na pamięci.
"""

from __future__ import annotations

import pytest
from fastapi import status
from fastapi.testclient import TestClient
from prometheus_client import REGISTRY

from tests.data import SAMPLE_QUERY

REQUEST_METRIC = "docfind_http_requests_total"


def requests_counted(route: str, method: str = "GET", code: str = "200") -> float:
    value = REGISTRY.get_sample_value(
        REQUEST_METRIC, {"method": method, "route": route, "status": code}
    )
    return value or 0.0


def test_metrics_endpoint_exposes_prometheus_format(client: TestClient) -> None:
    response = client.get("/metrics")

    assert response.status_code == status.HTTP_200_OK
    assert response.headers["content-type"].startswith("text/plain")


def test_search_requests_are_counted(client: TestClient) -> None:
    before = requests_counted("/search")

    client.get("/search", params={"q": SAMPLE_QUERY})
    client.get("/search", params={"q": SAMPLE_QUERY})

    assert requests_counted("/search") == before + 2


def test_latency_is_observed_for_search(client: TestClient) -> None:
    metric = "docfind_http_request_duration_seconds_count"
    labels = {"method": "GET", "route": "/search"}
    before = REGISTRY.get_sample_value(metric, labels) or 0.0

    client.get("/search", params={"q": SAMPLE_QUERY})

    assert REGISTRY.get_sample_value(metric, labels) == before + 1


@pytest.mark.parametrize("route", ["/healthz", "/readyz", "/version", "/metrics"])
def test_diagnostic_routes_are_not_counted(client: TestClient, route: str) -> None:
    """Sondy kubeleta pukają co kilka sekund i zdominowałyby histogram.

    p95 liczone razem z /healthz nie mówi nic o czasie wyszukiwania.
    """
    before = requests_counted(route)

    client.get(route)

    assert requests_counted(route) == before


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
