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

RELEASE_BUILD: Final = BuildInfo(
    version="1.2.3",
    commit=COMMIT_SHA,
    source_date="2026-09-22T10:00:00Z",
)
"""Poprawna, kompletna tożsamość builda."""

MALFORMED_VERSION_JSON: Final = "{to nie jest json"
"""Uszkodzony plik — na przykład przerwany zapis."""

NON_OBJECT_VERSION_JSON: Final = "[1, 2, 3]"
"""Poprawny JSON, ale nie obiekt — nie da się z niego odczytać pól."""

PARTIAL_BUILD: Final[dict[str, Any]] = {"version": "9.9.9"}
"""Tożsamość bez commita i czasu budowania."""

SAMPLE_QUERY: Final = "kubernetes"

MINIMAL_CONFIG_YAML: Final = """
elasticsearch:
  url: http://elasticsearch:9200
  alias: docfind
"""
"""Najmniejsza konfiguracja, jaka przechodzi walidację — reszta ma domyślne."""

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
