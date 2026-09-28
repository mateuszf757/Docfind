"""Endpointy diagnostyczne.

Wydzielone od funkcji produktu, bo rządzą się innymi regułami: nie wymagają
uwierzytelnienia, będą wyłączone z logu dostępowego w ingressie (Etap 3)
i nie wchodzą do metryk biznesowych (Etap 8).

Wszystkie są `async def` i nie mogą blokować. Handler `def` Starlette
wykonuje w puli wątków anyio, wspólnej dla całego procesu i ograniczonej do
40 wątków. Gdy zajmą ją wolne wywołania do Elasticsearcha albo modelu,
synchroniczne /healthz czeka na wolny wątek, przekracza timeout sondy
i kubelet restartuje pod, który był zdrowy — tylko zajęty. Handler
`async def` biegnie w pętli zdarzeń i odpowiada niezależnie od puli.
To samo dotyczy zależności: `def` w Depends też trafia do puli wątków.
"""

from __future__ import annotations

from typing import Annotated

from fastapi import APIRouter, Depends, Request, Response, status

from docfind_api.metrics import METRICS_PATH, HttpMetrics
from docfind_api.models import (
    BuildInfo,
    LivenessResponse,
    ReadinessCheck,
    ReadinessResponse,
)
from docfind_api.version import build_info

router = APIRouter(tags=["diagnostyka"])


async def http_metrics(request: Request) -> HttpMetrics:
    """Metryki tej instancji aplikacji (docfind_api.main.create_app)."""
    metrics: HttpMetrics = request.app.state.metrics
    return metrics


@router.get("/healthz", response_model=LivenessResponse)
async def healthz() -> LivenessResponse:
    """Proces żyje.

    Celowo nie dotyka zależności: gdyby sprawdzał Elasticsearch, awaria ES
    restartowałaby zdrowe pody API i zamieniała jedną awarię w dwie.
    """
    return LivenessResponse()


def _readiness_checks() -> list[ReadinessCheck]:
    """Zależności wymagane do obsługi ruchu.

    Pusto do Etapu 6, gdzie dochodzi alias w Elasticsearch, i Etapu 7
    z backendem LLM.
    """
    return []


@router.get("/readyz", response_model=ReadinessResponse)
async def readyz(response: Response) -> ReadinessResponse:
    """Gotowość na ruch.

    Niegotowość zwraca 503, żeby kubelet wyciął pod z endpointów Service
    zamiast kierować do niego żądania, które i tak się nie powiodą.
    """
    checks = _readiness_checks()
    ready = all(check.healthy for check in checks)

    if not ready:
        response.status_code = status.HTTP_503_SERVICE_UNAVAILABLE

    return ReadinessResponse(ready=ready, checks=checks)


@router.get("/version", response_model=BuildInfo)
async def version() -> BuildInfo:
    """Tożsamość działającego builda — fundament diagnostyki całego systemu."""
    return build_info()


@router.get(
    METRICS_PATH,
    response_class=Response,
    responses={200: {"content": {"text/plain": {}}}},
)
async def metrics(metrics: Annotated[HttpMetrics, Depends(http_metrics)]) -> Response:
    """Metryki dla Prometheusa.

    Wystawione bez uwierzytelnienia, bo w klastrze ruch do tej trasy nie
    wychodzi poza sieć podów — ingress nie kieruje tu żądań (Etap 3).
    """
    payload, content_type = metrics.render()
    return Response(content=payload, media_type=content_type)
