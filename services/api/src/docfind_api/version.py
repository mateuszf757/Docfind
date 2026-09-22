"""Tożsamość builda.

version.json powstaje wewnątrz obrazu przy budowaniu, z danych z gita.
Gdy go nie ma — bo uruchamiasz kod wprost z drzewa roboczego — zwracamy
jawne "unknown" zamiast zmyślać wersję. Endpoint /version jest podstawą
całej diagnostyki i nie wolno mu podawać wartości, której nie da się
powiązać z commitem.
"""

from __future__ import annotations

import json
import os
from functools import lru_cache
from pathlib import Path

VERSION_FILE_ENV = "DOCFIND_VERSION_FILE"
DEFAULT_VERSION_FILE = "/app/version.json"

UNKNOWN: dict[str, str] = {
    "version": "unknown",
    "commit": "unknown",
    "built_at": "unknown",
}


@lru_cache(maxsize=1)
def build_info() -> dict[str, str]:
    path = Path(os.environ.get(VERSION_FILE_ENV, DEFAULT_VERSION_FILE))
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return dict(UNKNOWN)

    return {key: str(raw.get(key, UNKNOWN[key])) for key in UNKNOWN}
