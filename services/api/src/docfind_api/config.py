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
from functools import lru_cache
from pathlib import Path
from typing import Any, Final

import yaml
from pydantic import AnyHttpUrl, BaseModel, ConfigDict, Field, ValidationError, model_validator

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
    shutdown_grace_seconds: float = Field(
        default=3.0,
        gt=0,
        description="Ile czekać na dokończenie żądań przy SIGTERM",
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

    @model_validator(mode="after")
    def _real_backend_requires_endpoint(self) -> LlmConfig:
        if self.backend is not LlmBackend.STUB and self.endpoint is None:
            raise ValueError(f"backend '{self.backend.value}' wymaga podania 'endpoint'")
        return self


class AppConfig(StrictModel):
    service: ServiceConfig = Field(default_factory=ServiceConfig)
    elasticsearch: ElasticsearchConfig
    llm: LlmConfig = Field(default_factory=LlmConfig)


def config_path() -> Path:
    return Path(os.environ.get(CONFIG_FILE_ENV, DEFAULT_CONFIG_FILE))


def _describe(error: dict[str, Any]) -> str:
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


@lru_cache(maxsize=1)
def get_config() -> AppConfig:
    """Konfiguracja procesu. Cache'owana — plik nie zmienia się w trakcie jego życia."""
    return load_config()
