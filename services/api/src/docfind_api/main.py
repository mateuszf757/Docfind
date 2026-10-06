"""Złożenie aplikacji DOCFIND API.

Ten moduł tylko montuje routery i middleware. Logika endpointów mieszka
w docfind_api.routers, kontrakt odpowiedzi w docfind_api.models,
konfiguracja w docfind_api.config.

Aplikacja powstaje w fabryce z gotową, zwalidowaną konfiguracją, a nie jako
obiekt modułu. Moduł na poziomie importu nie może znać konfiguracji — każda
zależność od niej musiałaby sięgać po globalny cache, a testy musiałyby go
czyścić. Tu konfiguracja wchodzi argumentem i ląduje w app.state; to samo
miejsce przyjmie klientów Elasticsearcha i modelu, zakładanych w lifespan.
"""

from __future__ import annotations

from fastapi import FastAPI

from docfind_api.config import AppConfig
from docfind_api.metrics import HttpMetrics, MetricsMiddleware
from docfind_api.request_id import RequestIdMiddleware
from docfind_api.routers import diagnostics, search


def create_app(config: AppConfig) -> FastAPI:
    app = FastAPI(
        title="DOCFIND API",
        summary="Wyszukiwarka dokumentów z odpowiedziami generowanymi przez model",
        # Swagger UI pobiera JS i CSS z cdn.jsdelivr.net — w przeglądarce
        # użytkownika klienta wykonywałby się kod z obcego serwera, a w sieci
        # bez dostępu do CDN strona i tak nie działa. /openapi.json zostaje.
        docs_url="/docs" if config.service.docs else None,
        redoc_url=None,
    )

    metrics = HttpMetrics()
    app.state.config = config
    app.state.metrics = metrics

    app.add_middleware(MetricsMiddleware, metrics=metrics)
    # Dodany jako ostatni, więc obejmuje wszystko inne — także odpowiedzi 404
    # i 422, które powstają przed routerami, i błędy z innych middleware.
    app.add_middleware(RequestIdMiddleware)

    app.include_router(diagnostics.router)
    app.include_router(search.router)
    return app
