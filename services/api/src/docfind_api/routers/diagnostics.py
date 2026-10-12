"""Endpointy diagnostyczne.

Wydzielone od funkcji produktu, bo rządzą się innymi regułami: nie wymagają
uwierzytelnienia (poza /metrics, gdy konfiguracja wskazuje sekret z
tokenami), będą wyłączone z logu dostępowego w ingressie (Etap 3) i nie
wchodzą do metryk biznesowych (Etap 8).

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

from fastapi import APIRouter, Depends, HTTPException, Request, Response, status

from docfind_api import tokens
from docfind_api.config import AppConfig
from docfind_api.metrics import METRICS_PATH, HttpMetrics
from docfind_api.models import (
    BuildInfo,
    LivenessResponse,
    ReadinessCheck,
    ReadinessResponse,
)
from docfind_api.secrets import SecretNotFound
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


async def require_metrics_token(request: Request) -> None:
    """Token z nagłówka Authorization musi być w sekrecie z metrics.token_secret.

    Bez tokenu albo ze złym — 401 z wyzwaniem Bearer. Sekret nieczytelny —
    503: metryki są wtedy niedostępne, a nie otwarte.

    `async def` jak reszta modułu, mimo synchronicznego odczytu pliku: wolumen
    Secreta to tmpfs, a plik ma kilkadziesiąt bajtów — to odczyt z pamięci,
    nie czekanie na dysk. Zależność `def` trafiłaby do puli wątków.
    """
    config: AppConfig = request.app.state.config
    secret_name = config.metrics.token_secret
    if secret_name is None:
        return
    presented = tokens.bearer(request.headers.get("authorization"))
    try:
        accepted = tokens.accepted_tokens(secret_name)
    except SecretNotFound as exc:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail="tokeny dostępu do metryk niedostępne",
        ) from exc
    if presented is None or not tokens.authorized(presented, accepted):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="wymagany token dostępu do metryk",
            headers={"WWW-Authenticate": "Bearer"},
        )


@router.get("/version", response_model=BuildInfo)
async def version() -> BuildInfo:
    """Tożsamość działającego builda — fundament diagnostyki całego systemu."""
    return build_info()


@router.get(
    METRICS_PATH,
    response_class=Response,
    responses={200: {"content": {"text/plain": {}}}},
)
async def metrics(
    metrics: Annotated[HttpMetrics, Depends(http_metrics)],
    _: Annotated[None, Depends(require_metrics_token)],
) -> Response:
    """Metryki dla Prometheusa.

    Trasa nie jest wystawiona przez Gateway (route.paths w charcie), a w
    klastrze wymaga tokenu, gdy konfiguracja go wskazuje. Do Etapu 4 była
    wystawiona publicznie — założenie „ingress tu nie kieruje" przestało być
    prawdą, gdy HTTPRoute dostał PathPrefix "/".
    """
    payload, content_type = metrics.render()
    return Response(content=payload, media_type=content_type)
