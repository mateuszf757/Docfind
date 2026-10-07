"""Wyszukiwanie — właściwa funkcja produktu.

Zaślepka do Etapu 6, który podpina Elasticsearch, i Etapu 7, który dokłada
strumieniowaną odpowiedź modelu.

Handler jest `def` celowo, w przeciwieństwie do diagnostyki. Dopóki klient
Elasticsearcha i modelu nie jest asynchroniczny, blokujące wywołanie w
`async def` zatrzymałoby pętlę zdarzeń całego procesu, razem z sondami;
w `def` zajmuje tylko jeden z wątków puli. Przejście na `async def` idzie
w parze z asynchronicznym klientem, nie wcześniej.

Zaślepka odpowiada natychmiast, chyba że konfiguracja każe jej udawać czas
odpowiedzi modelu (llm.stub_delay_ms). Bez tego żadne żądanie nie trwa
dłużej niż ułamek milisekundy, więc nie da się sprawdzić, czy proces dokańcza
żądania w locie przy SIGTERM — a przy strumieniowaniu z Etapu 7 to będzie
zwykły przypadek, nie wyjątek.
"""

from __future__ import annotations

import time
from typing import Annotated

from fastapi import APIRouter, Query, Request

from docfind_api.config import AppConfig, LlmBackend
from docfind_api.models import SearchResponse

router = APIRouter(tags=["wyszukiwanie"])


@router.get("/search", response_model=SearchResponse)
def search(
    request: Request,
    q: Annotated[str, Query(min_length=1, description="Zapytanie użytkownika")],
) -> SearchResponse:
    config: AppConfig = request.app.state.config
    if config.llm.backend is LlmBackend.STUB and config.llm.stub_delay_ms > 0:
        # Handler `def` biegnie w puli wątków, więc sen blokuje jeden wątek,
        # a nie pętlę zdarzeń — sondy odpowiadają w tym czasie normalnie.
        time.sleep(config.llm.stub_delay_ms / 1000)
    return SearchResponse(query=q)
