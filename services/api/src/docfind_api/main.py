"""Złożenie aplikacji DOCFIND API.

Ten moduł tylko montuje routery i middleware. Logika endpointów mieszka
w docfind_api.routers, kontrakt odpowiedzi w docfind_api.models,
konfiguracja w docfind_api.config.
"""

from __future__ import annotations

from fastapi import FastAPI

from docfind_api.metrics import MetricsMiddleware
from docfind_api.routers import diagnostics, search

app = FastAPI(
    title="DOCFIND API",
    summary="Wyszukiwarka dokumentów z odpowiedziami generowanymi przez model",
    docs_url="/docs",
    redoc_url=None,
)

app.add_middleware(MetricsMiddleware)

app.include_router(diagnostics.router)
app.include_router(search.router)
