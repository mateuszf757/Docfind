"""Narzędzie konfiguracji: walidacja app.yml modelem aplikacji i schemat JSON.

    python -m docfind_api.configtool check <app.yml>
    python -m docfind_api.configtool schema --write <ścieżka>
    python -m docfind_api.configtool schema --check <ścieżka>

Tym samym modelem, którym aplikacja waliduje konfigurację przy starcie.
Bramka w CI (konfiguracja z wyrenderowanego charta) i administrator klienta
(`docker run … python -m docfind_api.configtool check /app/config/app.yml`)
dostają ten sam werdykt, co proces, który miałby z nią wstać.

Wcześniej ten kod siedział we wstawkach `python -c` w skryptach bashowych —
bez typów i testów, niewidoczny dla ruff i mypy (decyzja 23).

Kody wyjścia: 0 — konfiguracja poprawna albo schemat aktualny, 1 — niepoprawna
albo nieaktualny, 2 — złe wywołanie.
"""

from __future__ import annotations

import argparse
import difflib
import json
import sys
from pathlib import Path

from docfind_api.config import AppConfig, ConfigError, load_config

SCHEMA_DIALECT = "https://json-schema.org/draft/2020-12/schema"
SCHEMA_TITLE = "Konfiguracja DOCFIND"


def schema_text() -> str:
    """Schemat app.yml wygenerowany z modelu, w postaci zapisywanej do repozytorium."""
    schema = AppConfig.model_json_schema()
    schema["$schema"] = SCHEMA_DIALECT
    schema["title"] = SCHEMA_TITLE
    return json.dumps(schema, indent=2, ensure_ascii=False) + "\n"


def _check_config(path: Path) -> int:
    try:
        load_config(path)
    except ConfigError as exc:
        print(f"BŁĄD KONFIGURACJI\n{exc}", file=sys.stderr)
        return 1
    print(f"==> {path}: konfiguracja poprawna")
    return 0


def _check_schema(path: Path) -> int:
    expected = schema_text()
    current = path.read_text(encoding="utf-8") if path.exists() else ""
    if current == expected:
        print("==> schemat aktualny")
        return 0
    print(
        f"NIESPEŁNIONE: {path} jest nieaktualny wobec modelu — "
        "uruchom ./bin/mise run schema i zacommituj wynik",
        file=sys.stderr,
    )
    sys.stderr.writelines(
        difflib.unified_diff(
            current.splitlines(keepends=True),
            expected.splitlines(keepends=True),
            fromfile=f"{path} (w repozytorium)",
            tofile="z modelu",
        )
    )
    return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m docfind_api.configtool",
        description="Walidacja app.yml modelem aplikacji i schemat JSON konfiguracji.",
    )
    commands = parser.add_subparsers(dest="command", required=True)
    check = commands.add_parser("check", help="sprawdź plik konfiguracji modelem aplikacji")
    check.add_argument("path", type=Path)
    schema = commands.add_parser("schema", help="zapisz albo sprawdź app.schema.json")
    mode = schema.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", type=Path, metavar="ŚCIEŻKA")
    mode.add_argument("--check", type=Path, metavar="ŚCIEŻKA")
    args = parser.parse_args(argv)

    if args.command == "check":
        return _check_config(args.path)
    if args.write is not None:
        args.write.write_text(schema_text(), encoding="utf-8")
        print(f"==> zapisano {args.write}")
        return 0
    return _check_schema(args.check)


if __name__ == "__main__":
    raise SystemExit(main())
