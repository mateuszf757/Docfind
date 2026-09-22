"""Kontrakt API wyrażony typami.

Endpointy deklarują `response_model`, więc kształt odpowiedzi jest sprawdzany
przy każdym zwrocie i trafia do schematu OpenAPI. Słownik zwrócony wprost
z handlera milcząco zmieniłby kontrakt — model na to nie pozwala.
"""

from __future__ import annotations

from typing import Final, Literal

from pydantic import BaseModel, ConfigDict, Field

UNKNOWN: Final = "unknown"
"""Znacznik pola tożsamości, którego nie dało się odczytać.

Jawny znacznik zamiast zgadywania: /version jest podstawą diagnostyki i nie
wolno mu podać wersji, której nie da się powiązać z commitem.
"""


class BuildInfo(BaseModel):
    """Tożsamość builda — co to za wersja i z jakiego commita powstała.

    Model jest niezmienny, bo instancja jest współdzielona przez cache
    w `docfind_api.version.build_info`.
    """

    model_config = ConfigDict(frozen=True)

    version: str = Field(description="Wersja z git describe", examples=["1.2.3"])
    commit: str = Field(description="Pełne sha commita", examples=["0123456789abcdef"])
    built_at: str = Field(description="Czas budowania w UTC", examples=["2026-09-22T10:00:00Z"])


UNKNOWN_BUILD: Final = BuildInfo(version=UNKNOWN, commit=UNKNOWN, built_at=UNKNOWN)
"""Odpowiedź, gdy tożsamości nie da się ustalić — kod biegnie spoza obrazu."""


class LivenessResponse(BaseModel):
    """Odpowiedź /healthz — proces żyje."""

    status: Literal["ok"] = "ok"


class ReadinessCheck(BaseModel):
    """Pojedyncza zależność sprawdzana przed dopuszczeniem ruchu."""

    name: str
    healthy: bool
    detail: str | None = None


class ReadinessResponse(BaseModel):
    """Odpowiedź /readyz — czy usługa może przyjmować ruch."""

    ready: bool
    checks: list[ReadinessCheck] = Field(default_factory=list)


class Fragment(BaseModel):
    """Fragment dokumentu zwrócony z indeksu."""

    document: str = Field(description="Ścieżka dokumentu źródłowego")
    excerpt: str = Field(description="Treść fragmentu")
    score: float = Field(description="Trafność nadana przez Elasticsearch")


class SearchResponse(BaseModel):
    """Odpowiedź /search — fragmenty z indeksu i odpowiedź modelu."""

    query: str
    fragments: list[Fragment] = Field(default_factory=list)
    answer: str | None = Field(
        default=None,
        description="Odpowiedź modelu; None dopóki backend LLM nie jest podpięty",
    )
