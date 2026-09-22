"""Dane testowe.

Wydzielone z asercji, bo literał powtórzony w kilku plikach staje się cichym
kontraktem — poprawiony w jednym miejscu zostawia resztę nieaktualną.
Budujemy je z modeli produkcyjnych, więc zmiana kontraktu psuje testy tutaj,
a nie w kilkunastu asercjach naraz.
"""

from __future__ import annotations

from typing import Any, Final

from docfind_api.models import BuildInfo

COMMIT_SHA: Final = "0123456789abcdef0123456789abcdef01234567"

RELEASE_BUILD: Final = BuildInfo(
    version="1.2.3",
    commit=COMMIT_SHA,
    built_at="2026-09-22T10:00:00Z",
)
"""Poprawna, kompletna tożsamość builda."""

MALFORMED_VERSION_JSON: Final = "{to nie jest json"
"""Uszkodzony plik — na przykład przerwany zapis."""

NON_OBJECT_VERSION_JSON: Final = "[1, 2, 3]"
"""Poprawny JSON, ale nie obiekt — nie da się z niego odczytać pól."""

PARTIAL_BUILD: Final[dict[str, Any]] = {"version": "9.9.9"}
"""Tożsamość bez commita i czasu budowania."""

SAMPLE_QUERY: Final = "kubernetes"
