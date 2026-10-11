"""Wczytywanie tożsamości builda.

version.json powstaje wewnątrz obrazu przy budowaniu i niesie tylko to, co
wynika z commita: commit i jego czas (tools/internal/stages/build). Wersję
podaje wdrożenie zmienną DOCFIND_VERSION — wydanie X.Y.Z na produkcji, wersja
deweloperska z gita gdzie indziej — bo ten sam obraz przechodzi przez
staging i trafia na produkcję (decyzja 40). Gdy czegoś brakuje — kod biegnie
wprost z drzewa roboczego albo wdrożenie nie podało wersji — pole dostaje
UNKNOWN zamiast zmyślonej wartości.
"""

from __future__ import annotations

import json
import os
from functools import lru_cache
from pathlib import Path
from typing import Final

from docfind_api.models import UNKNOWN, UNKNOWN_BUILD, BuildInfo

VERSION_FILE_ENV: Final = "DOCFIND_VERSION_FILE"
DEFAULT_VERSION_FILE: Final = Path("/app/version.json")
VERSION_ENV: Final = "DOCFIND_VERSION"
"""Wersja wdrożenia, ustawiana przez chart (api.version)."""


def _version_file() -> Path:
    """Ścieżka do version.json. Nadpisywalna zmienną środowiskową na potrzeby testów."""
    return Path(os.environ.get(VERSION_FILE_ENV, DEFAULT_VERSION_FILE))


def _deployed_version() -> str:
    """Wersja z wdrożenia; pusta zmienna to brak wersji, nie wersja pusta."""
    return os.environ.get(VERSION_ENV, "").strip() or UNKNOWN


@lru_cache(maxsize=1)
def build_info() -> BuildInfo:
    """Tożsamość tego builda i wdrożenia.

    Cache'owane: plik powstaje przy budowaniu obrazu, a zmienna przy starcie
    poda, więc żadne nie zmienia się w trakcie życia procesu. Testy czyszczą
    cache przez `build_info.cache_clear()`.
    """
    version = _deployed_version()
    try:
        document = json.loads(_version_file().read_text(encoding="utf-8"))
    except OSError, json.JSONDecodeError:
        return UNKNOWN_BUILD.model_copy(update={"version": version})

    if not isinstance(document, dict):
        return UNKNOWN_BUILD.model_copy(update={"version": version})

    # Brakujące pole uzupełniamy znacznikiem zamiast odrzucać cały plik —
    # częściowa tożsamość jest bardziej użyteczna niż żadna.
    return BuildInfo(
        version=version,
        commit=str(document.get("commit", UNKNOWN)),
        source_date=str(document.get("source_date", UNKNOWN)),
    )
