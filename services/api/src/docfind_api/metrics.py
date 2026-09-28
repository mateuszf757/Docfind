"""Metryki Prometheusa.

Trzy decyzje warte zapamiętania.

Etykieta trasy jest szablonem ('/search'), nie ścieżką żądania. Surowa
ścieżka wysadziłaby kardynalność — każde inne zapytanie tworzyłoby nowy
szereg czasowy i Prometheus przewróciłby się na pamięci.

Middleware jest czystym ASGI, nie BaseHTTPMiddleware. To drugie buforuje
odpowiedź i psuje strumieniowanie, którego potrzebujemy na Etapie 7.

Każda aplikacja ma własny rejestr zamiast globalnego REGISTRY. Globalny
rejestr żył tyle co proces, więc testy musiały liczyć przyrosty zamiast
wartości, a druga instancja aplikacji w tym samym procesie dopisywałaby się
do cudzych liczników. Kolektory procesu, platformy i GC globalny rejestr
dawał sam — tu są rejestrowane jawnie, inaczej /metrics straciłoby pamięć,
CPU i deskryptory procesu.
"""

from __future__ import annotations

import time
from typing import Final

from prometheus_client import (
    CONTENT_TYPE_LATEST,
    CollectorRegistry,
    Counter,
    GCCollector,
    Histogram,
    PlatformCollector,
    ProcessCollector,
    generate_latest,
)
from starlette.routing import Route
from starlette.types import ASGIApp, Message, Receive, Scope, Send

METRICS_PATH: Final = "/metrics"

EXCLUDED_ROUTES: Final = frozenset({"/healthz", "/readyz", "/version", METRICS_PATH})
"""Trasy poza metrykami żądań.

Sondy kubeleta pukają co kilka sekund i zdominowałyby zarówno licznik, jak
i histogram — p95 liczone razem z /healthz nie mówi nic o czasie wyszukiwania.
"""


class HttpMetrics:
    """Metryki HTTP jednej instancji aplikacji, we własnym rejestrze."""

    def __init__(self) -> None:
        self.registry = CollectorRegistry()
        ProcessCollector(registry=self.registry)
        PlatformCollector(registry=self.registry)
        GCCollector(registry=self.registry)

        self.requests = Counter(
            "docfind_http_requests_total",
            "Liczba żądań HTTP według trasy i statusu",
            ["method", "route", "status"],
            registry=self.registry,
        )
        self.latency = Histogram(
            "docfind_http_request_duration_seconds",
            "Czas obsługi żądania HTTP",
            ["method", "route"],
            registry=self.registry,
        )

    def render(self) -> tuple[bytes, str]:
        """Treść odpowiedzi /metrics i jej typ zawartości."""
        return generate_latest(self.registry), CONTENT_TYPE_LATEST


def _route_template(scope: Scope) -> str | None:
    """Szablon dopasowanej trasy albo None, gdy żądanie nie trafiło w żadną."""
    route = scope.get("route")
    return route.path if isinstance(route, Route) else None


class MetricsMiddleware:
    """Zlicza żądania i mierzy czas ich obsługi."""

    def __init__(self, app: ASGIApp, metrics: HttpMetrics) -> None:
        self.app = app
        self.metrics = metrics

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        status_code = 500
        started = time.perf_counter()

        async def observe_status(message: Message) -> None:
            nonlocal status_code
            if message["type"] == "http.response.start":
                status_code = message["status"]
            await send(message)

        try:
            await self.app(scope, receive, observe_status)
        finally:
            elapsed = time.perf_counter() - started
            route = _route_template(scope)

            # Brak dopasowania (404) też pomijamy — inaczej skan katalogów
            # stworzyłby szereg czasowy na każdy zgadywany adres.
            if route is not None and route not in EXCLUDED_ROUTES:
                method = scope.get("method", "UNKNOWN")
                self.metrics.requests.labels(
                    method=method, route=route, status=str(status_code)
                ).inc()
                self.metrics.latency.labels(method=method, route=route).observe(elapsed)
