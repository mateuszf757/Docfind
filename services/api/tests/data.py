"""Dane testowe.

Wydzielone z asercji, bo literał powtórzony w kilku plikach staje się cichym
kontraktem — poprawiony w jednym miejscu zostawia resztę nieaktualną.
Budujemy je z modeli produkcyjnych, więc zmiana kontraktu psuje testy tutaj,
a nie w kilkunastu asercjach naraz.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any, Final

from docfind_api.models import BuildInfo

COMMIT_SHA: Final = "0123456789abcdef0123456789abcdef01234567"

BUILD_FILE: Final[dict[str, Any]] = {
    "commit": COMMIT_SHA,
    "source_date": "2026-09-22T10:00:00Z",
}
"""version.json z obrazu — tylko to, co wynika z commita (decyzja 40)."""

DEPLOYED_VERSION: Final = "1.2.3"
"""Wersja, którą wdrożenie podaje w DOCFIND_VERSION."""

RELEASE_BUILD: Final = BuildInfo(version=DEPLOYED_VERSION, **BUILD_FILE)
"""Poprawna, kompletna tożsamość: obraz z version.json, wdrożony jako wydanie."""

LEGACY_BUILD_FILE: Final[dict[str, Any]] = {**BUILD_FILE, "version": "0.3.0"}
"""version.json z obrazu sprzed decyzji 40, który niósł też wersję."""

MALFORMED_VERSION_JSON: Final = "{to nie jest json"
"""Uszkodzony plik — na przykład przerwany zapis."""

NON_OBJECT_VERSION_JSON: Final = "[1, 2, 3]"
"""Poprawny JSON, ale nie obiekt — nie da się z niego odczytać pól."""

PARTIAL_BUILD: Final[dict[str, Any]] = {"commit": COMMIT_SHA}
"""Tożsamość bez czasu budowania."""

SAMPLE_QUERY: Final = "kubernetes"

MINIMAL_CONFIG_YAML: Final = """
elasticsearch:
  url: http://elasticsearch:9200
  alias: docfind
"""
"""Najmniejsza konfiguracja, jaka przechodzi walidację — reszta ma domyślne."""

CONFIG_WITH_STUB_DELAY_YAML: Final = """
elasticsearch:
  url: http://elasticsearch:9200
  alias: docfind
llm:
  stub_delay_ms: 1500
"""
"""Zaślepka udająca czas odpowiedzi modelu — dla bramek żądania w locie."""

CONFIG_WITH_DELAY_ON_REAL_BACKEND_YAML: Final = """
elasticsearch:
  url: http://elasticsearch:9200
  alias: docfind
llm:
  backend: ollama
  endpoint: http://ollama:11434
  stub_delay_ms: 100
"""
"""Opóźnienie zaślepki przy prawdziwym backendzie — ma być odrzucone."""

CONFIG_WITH_UNKNOWN_KEY_YAML: Final = """
elasticsearch:
  url: http://elasticsearch:9200
  alias: docfind
service:
  log_levl: info
"""
"""Literówka w nazwie klucza — ma wysadzić start, nie zostać zignorowana."""

CONFIG_WITH_REAL_BACKEND_YAML: Final = """
elasticsearch:
  url: http://elasticsearch:9200
  alias: docfind
llm:
  backend: ollama
"""
"""Backend inny niż stub bez wymaganego endpointu."""

MALFORMED_CONFIG_YAML: Final = "service:\n  port: [nie\n"

CONFIG_WITH_FRACTIONAL_GRACE_YAML: Final = """
elasticsearch:
  url: http://elasticsearch:9200
  alias: docfind
service:
  shutdown_grace_seconds: 0.5
"""
"""Ułamek sekundy — uvicorn przyjmuje liczbę całkowitą i int() zrobiłby z tego zero."""

SECRET_NAME: Final = "elasticsearch_password"
SECRET_VALUE: Final = "tajne-haslo"

EXAMPLE_CONFIG_PATH: Final = (
    Path(__file__).resolve().parents[3] / "deploy" / "config" / "app.yml.example"
)
"""Przykład dostarczany z produktem. Test pilnuje, żeby nie rozjechał się z modelem."""

ENVOY_REQUEST_ID: Final = "8f14e45f-ceea-467a-9a2e-4d2a1d3e9b1c"
"""Identyfikator w formacie, który nadaje Envoy (UUID v4)."""

UNSAFE_REQUEST_IDS: Final = (
    "abc def",
    "a" * 129,
    "id;rm -rf",
    "żółw",
)
"""Wartości spoza dozwolonego formatu — nie mogą trafić do odpowiedzi ani do logu.
Spacja, za długi, znaki spoza zbioru, znaki spoza ASCII.
"""

# Tokeny dostępu do /metrics (docfind_api.tokens). Wartości bez znaczenia
# poza testem; dwa, bo rotacja to chwila, w której ważne są oba.
METRICS_TOKEN_SECRET: Final = "metrics-token"
METRICS_TOKEN_OLD: Final = "stary-token-metryk"
METRICS_TOKEN_NEW: Final = "nowy-token-metryk"
# Nagłówki HTTP to latin-1 — token spoza ASCII nie dojdzie nawet do serwera.
METRICS_TOKEN_WRONG: Final = "zly-token-metryk"

CONFIG_WITH_METRICS_TOKEN: Final[dict[str, Any]] = {
    "elasticsearch": {"url": "http://elasticsearch:9200", "alias": "docfind"},
    "metrics": {"token_secret": METRICS_TOKEN_SECRET},
}
