"""Wyszukiwanie — właściwa funkcja produktu.

Zaślepka do Etapu 6, który podpina Elasticsearch, i Etapu 7, który dokłada
strumieniowaną odpowiedź modelu.
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
