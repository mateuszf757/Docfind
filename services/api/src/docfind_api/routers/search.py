"""Wyszukiwanie — właściwa funkcja produktu.

Zaślepka do Etapu 6, który podpina Elasticsearch, i Etapu 7, który dokłada
strumieniowaną odpowiedź modelu.

Handler jest `def` celowo, w przeciwieństwie do diagnostyki. Dopóki klient
Elasticsearcha i modelu nie jest asynchroniczny, blokujące wywołanie w
`async def` zatrzymałoby pętlę zdarzeń całego procesu, razem z sondami;
w `def` zajmuje tylko jeden z wątków puli. Przejście na `async def` idzie
w parze z asynchronicznym klientem, nie wcześniej.
"""

from __future__ import annotations

from typing import Annotated

from fastapi import APIRouter, Query

from docfind_api.models import SearchResponse

router = APIRouter(tags=["wyszukiwanie"])


@router.get("/search", response_model=SearchResponse)
def search(
    q: Annotated[str, Query(min_length=1, description="Zapytanie użytkownika")],
) -> SearchResponse:
    return SearchResponse(query=q)
