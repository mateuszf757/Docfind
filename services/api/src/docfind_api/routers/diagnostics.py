"""Endpointy diagnostyczne.

Wydzielone od funkcji produktu, bo rządzą się innymi regułami: nie wymagają
uwierzytelnienia, będą wyłączone z logu dostępowego w ingressie (Etap 3)
i nie wchodzą do metryk biznesowych (Etap 8).
"""

from __future__ import annotations

from fastapi import APIRouter, Response, status

from docfind_api.models import (
    BuildInfo,
    LivenessResponse,
    ReadinessCheck,
    ReadinessResponse,
)
from docfind_api.version import build_info

router = APIRouter(tags=["diagnostyka"])


@router.get("/healthz", response_model=LivenessResponse)
def healthz() -> LivenessResponse:
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
def readyz(response: Response) -> ReadinessResponse:
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
def version() -> BuildInfo:
    """Tożsamość działającego builda — fundament diagnostyki całego systemu."""
    return build_info()
