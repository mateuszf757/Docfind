"""Testy narzędzia konfiguracji.

Tym narzędziem bramka w CI waliduje konfigurację z charta, a administrator
klienta swoją — werdykt musi być ten sam, co przy starcie aplikacji.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from docfind_api import configtool
from tests.data import CONFIG_WITH_UNKNOWN_KEY_YAML, EXAMPLE_CONFIG_PATH

SCHEMA_PATH = EXAMPLE_CONFIG_PATH.parent / "app.schema.json"


def test_example_config_passes(capsys: pytest.CaptureFixture[str]) -> None:
    assert configtool.main(["check", str(EXAMPLE_CONFIG_PATH)]) == 0
    assert "konfiguracja poprawna" in capsys.readouterr().out


def test_invalid_config_names_the_field(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path / "app.yml"
    path.write_text(CONFIG_WITH_UNKNOWN_KEY_YAML, encoding="utf-8")

    assert configtool.main(["check", str(path)]) == 1

    err = capsys.readouterr().err
    assert "BŁĄD KONFIGURACJI" in err
    assert "service.log_levl" in err


def test_committed_schema_is_current() -> None:
    """Schemat w repozytorium to dokładnie ten wygenerowany z modelu."""
    assert configtool.main(["schema", "--check", str(SCHEMA_PATH)]) == 0


def test_stale_schema_is_rejected(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    stale = tmp_path / "app.schema.json"
    stale.write_text(SCHEMA_PATH.read_text(encoding="utf-8").replace("DOCFIND", "STARY"), "utf-8")

    assert configtool.main(["schema", "--check", str(stale)]) == 1
    assert "nieaktualny" in capsys.readouterr().err


def test_schema_write_produces_checked_content(tmp_path: Path) -> None:
    target = tmp_path / "app.schema.json"

    assert configtool.main(["schema", "--write", str(target)]) == 0
    assert target.read_text(encoding="utf-8") == configtool.schema_text()


def test_usage_error_exits_with_two() -> None:
    with pytest.raises(SystemExit) as exc:
        configtool.main(["schema"])
    assert exc.value.code == 2
