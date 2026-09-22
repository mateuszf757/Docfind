"""Wczytywanie tożsamości builda.

version.json powstaje wewnątrz obrazu przy budowaniu, z danych z gita
(patrz ci/lib.sh). Gdy pliku nie ma — bo kod biegnie wprost z drzewa
roboczego — zwracamy UNKNOWN_BUILD zamiast zmyślać wersję.
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


def _version_file() -> Path:
    """Ścieżka do version.json. Nadpisywalna zmienną środowiskową na potrzeby testów."""
    return Path(os.environ.get(VERSION_FILE_ENV, DEFAULT_VERSION_FILE))


@lru_cache(maxsize=1)
def build_info() -> BuildInfo:
    """Tożsamość tego builda.

    Cache'owane: plik nie zmienia się w trakcie życia procesu, bo powstaje
    przy budowaniu obrazu. Testy czyszczą cache przez `build_info.cache_clear()`.
    """
    try:
        document = json.loads(_version_file().read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return UNKNOWN_BUILD

    if not isinstance(document, dict):
        return UNKNOWN_BUILD

    # Brakujące pole uzupełniamy znacznikiem zamiast odrzucać cały plik —
    # częściowa tożsamość jest bardziej użyteczna niż żadna.
    return BuildInfo(
        version=str(document.get("version", UNKNOWN)),
        commit=str(document.get("commit", UNKNOWN)),
        built_at=str(document.get("built_at", UNKNOWN)),
    )
