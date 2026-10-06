"""Konfiguracja aplikacji.

Model Pydantic jest jedynym źródłem prawdy o kształcie konfiguracji —
deploy/config/app.schema.json jest z niego generowany, więc schemat
i walidacja nie mogą się rozjechać.

Aplikacja odmawia startu przy nieprawidłowej konfiguracji. Usługa, która
wstanie z połowiczną konfiguracją, przewróci się później i w trudniejszym
do zdiagnozowania miejscu.
"""

from __future__ import annotations

import os
from enum import StrEnum
from pathlib import Path
from typing import Final

import yaml
from pydantic import AnyHttpUrl, BaseModel, ConfigDict, Field, ValidationError, model_validator
from pydantic_core import ErrorDetails

CONFIG_FILE_ENV: Final = "DOCFIND_CONFIG"
DEFAULT_CONFIG_FILE: Final = Path("/app/config/app.yml")


class ConfigError(Exception):
    """Konfiguracja jest nieczytelna albo nieprawidłowa. Komunikat jest dla człowieka."""


class LogLevel(StrEnum):
    DEBUG = "debug"
    INFO = "info"
    WARNING = "warning"
    ERROR = "error"


class LlmBackend(StrEnum):
    """Backend modelu wybierany konfiguracją — klienci mają różny sprzęt.

    STUB daje deterministyczne wyjście i nie wymaga GPU, więc na nim stoją
    testy e2e w pipelinie (patrz docs/DECYZJE.md, decyzja 9).
    """

    STUB = "stub"
    OLLAMA = "ollama"
    VLLM = "vllm"


class StrictModel(BaseModel):
    """Wspólna baza: niezmienna i odrzucająca nieznane klucze.

    extra="forbid" jest tu istotne — literówka w nazwie klucza ma wysadzić
    start, a nie zostać po cichu zignorowana i objawić się jako "ustawienie
    nie działa" pół roku później.
    """

    model_config = ConfigDict(frozen=True, extra="forbid")


class ServiceConfig(StrictModel):
    host: str = Field(default="0.0.0.0", description="Adres nasłuchu")
    port: int = Field(default=8000, ge=1, le=65535)
    log_level: LogLevel = LogLevel.INFO
    # Liczba całkowita, bo tyle przyjmuje uvicorn. Wcześniej był tu float
    # z gt=0, a przekazanie robiło int(): 0,5 przechodziło walidację, stawało
    # się zerem i uvicorn anulował żądania w locie po 0,1 s zamiast je dokończyć.
    shutdown_grace_seconds: int = Field(
        default=3,
        ge=1,
        description="Ile sekund czekać na dokończenie żądań przy SIGTERM",
    )
    keep_alive_seconds: int = Field(
        default=5,
        ge=1,
        description=(
            "Ile sekund uvicorn trzyma bezczynne połączenie. Za proxy musi być dłuższy "
            "niż limit bezczynności puli połączeń do backendu w proxy — inaczej proxy "
            "trafia w połączenie, które uvicorn właśnie zamyka, i zwraca 502."
        ),
    )
    docs: bool = Field(
        default=False,
        description=(
            "Swagger UI pod /docs. Strona ładuje JS i CSS z cdn.jsdelivr.net, więc "
            "domyślnie jest wyłączona; /openapi.json działa zawsze."
        ),
    )


class ElasticsearchConfig(StrictModel):
    url: AnyHttpUrl
    alias: str = Field(
        min_length=1,
        description="Alias indeksu. API nigdy nie odwołuje się do nazwy indeksu wprost.",
    )
    timeout_seconds: float = Field(default=5.0, gt=0)


class LlmConfig(StrictModel):
    backend: LlmBackend = LlmBackend.STUB
    endpoint: AnyHttpUrl | None = None
    model: str | None = None
    max_context: int = Field(default=8192, gt=0, description="Limit kontekstu modelu w tokenach")
    max_fragments: int = Field(default=8, gt=0, description="Ile fragmentów trafia do promptu")
    stub_delay_ms: int = Field(
        default=0,
        ge=0,
        le=60_000,
        description=(
            "Sztuczne opóźnienie odpowiedzi zaślepki w milisekundach, tylko dla backendu stub. "
            "Bramki potrzebują żądania, które trwa: zamykanie przy SIGTERM ma je dokończyć, "
            "a drain ma przejść bez jego utraty. W produkcji 0."
        ),
    )

    @model_validator(mode="after")
    def _real_backend_requires_endpoint(self) -> LlmConfig:
        if self.backend is not LlmBackend.STUB and self.endpoint is None:
            raise ValueError(f"backend '{self.backend.value}' wymaga podania 'endpoint'")
        return self

    @model_validator(mode="after")
    def _delay_only_for_stub(self) -> LlmConfig:
        # Opóźnienie przy prawdziwym modelu nic by nie symulowało — dokładałoby
        # czas do prawdziwej odpowiedzi. Odrzucone, zanim ktoś zostawi je
        # w konfiguracji produkcyjnej po teście.
        if self.backend is not LlmBackend.STUB and self.stub_delay_ms:
            raise ValueError("stub_delay_ms dotyczy tylko backendu 'stub'")
        return self


class AppConfig(StrictModel):
    service: ServiceConfig = Field(default_factory=ServiceConfig)
    elasticsearch: ElasticsearchConfig
    llm: LlmConfig = Field(default_factory=LlmConfig)


def config_path() -> Path:
    return Path(os.environ.get(CONFIG_FILE_ENV, DEFAULT_CONFIG_FILE))


def _describe(error: ErrorDetails) -> str:
    location = ".".join(str(part) for part in error["loc"]) or "(korzeń)"
    return f"  {location}: {error['msg']}"


def load_config(path: Path | None = None) -> AppConfig:
    """Wczytuje i waliduje konfigurację.

    Każdy błąd jest zamieniany na ConfigError z komunikatem wskazującym
    konkretne pole — ślad stosu Pydantica nie mówi administratorowi klienta nic.
    """
    source = path or config_path()

    try:
        raw = yaml.safe_load(source.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise ConfigError(f"brak pliku konfiguracyjnego: {source}") from exc
    except OSError as exc:
        raise ConfigError(f"nie udało się odczytać {source}: {exc}") from exc
    except yaml.YAMLError as exc:
        raise ConfigError(f"{source} nie jest poprawnym YAML-em:\n  {exc}") from exc

    if raw is None:
        raise ConfigError(f"{source} jest pusty")
    if not isinstance(raw, dict):
        raise ConfigError(f"{source} musi zawierać mapowanie, a zawiera {type(raw).__name__}")

    try:
        return AppConfig.model_validate(raw)
    except ValidationError as exc:
        problems = "\n".join(_describe(error) for error in exc.errors())
        raise ConfigError(f"nieprawidłowa konfiguracja w {source}:\n{problems}") from exc
