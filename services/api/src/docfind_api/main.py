"""DOCFIND API — szkielet z Etapu 0.

Na tym etapie /search jest zaślepką, a /readyz nie ma jeszcze czego sprawdzać.
Wypełniają się w Etapie 6 (Elasticsearch) i 7 (LLM).
"""

from __future__ import annotations

from fastapi import FastAPI, Query

from .version import build_info

app = FastAPI(title="DOCFIND API", docs_url="/docs", redoc_url=None)


@app.get("/healthz")
def healthz() -> dict[str, str]:
    """Żyje. Nie dotyka zależności — inaczej awaria ES restartowałaby API."""
    return {"status": "ok"}


@app.get("/readyz")
def readyz() -> dict[str, str]:
    """Gotowe na ruch. Od Etapu 6 sprawdza tu alias w ES i backend LLM."""
    return {"status": "ready", "checks": "none"}


@app.get("/version")
def version() -> dict[str, str]:
    return build_info()


@app.get("/search")
def search(q: str = Query(..., min_length=1, description="Zapytanie")) -> dict[str, object]:
    return {"query": q, "fragments": [], "answer": None}
