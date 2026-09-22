"""Testy konfiguracji.

Sednem jest to, że aplikacja odmawia startu przy nieprawidłowej konfiguracji
i mówi, które pole jest złe. Usługa, która wstanie z połowiczną konfiguracją,
przewróci się później i w trudniejszym do zdiagnozowania miejscu.
"""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from docfind_api.config import (
    AppConfig,
    ConfigError,
    LlmBackend,
    LogLevel,
    load_config,
)
from tests.conftest import WriteConfigFile
from tests.data import (
    CONFIG_WITH_REAL_BACKEND_YAML,
    CONFIG_WITH_UNKNOWN_KEY_YAML,
    EXAMPLE_CONFIG_PATH,
    MALFORMED_CONFIG_YAML,
    MINIMAL_CONFIG_YAML,
)


def test_example_shipped_with_product_is_valid() -> None:
    """Przykład z deploy/config nie może rozjechać się z modelem.

    Administrator klienta zaczyna od skopiowania tego pliku — gdyby był
    nieprawidłowy, pierwszy start u klienta kończyłby się błędem.
    """
    assert isinstance(load_config(EXAMPLE_CONFIG_PATH), AppConfig)


def test_minimal_config_fills_defaults(given_config_file: WriteConfigFile) -> None:
    given_config_file(MINIMAL_CONFIG_YAML)

    config = load_config()

    assert config.service.port == 8000
    assert config.service.log_level is LogLevel.INFO
    assert config.llm.backend is LlmBackend.STUB
    assert config.elasticsearch.alias == "docfind"


def test_missing_file_names_the_path(given_config_file: WriteConfigFile, tmp_path) -> None:
    path = given_config_file(MINIMAL_CONFIG_YAML)
    path.unlink()

    with pytest.raises(ConfigError, match="brak pliku konfiguracyjnego"):
        load_config()


def test_empty_file_is_rejected(given_config_file: WriteConfigFile) -> None:
    given_config_file("")

    with pytest.raises(ConfigError, match="jest pusty"):
        load_config()


def test_malformed_yaml_is_rejected(given_config_file: WriteConfigFile) -> None:
    given_config_file(MALFORMED_CONFIG_YAML)

    with pytest.raises(ConfigError, match="nie jest poprawnym YAML-em"):
        load_config()


def test_non_mapping_is_rejected(given_config_file: WriteConfigFile) -> None:
    given_config_file("- to\n- jest\n- lista\n")

    with pytest.raises(ConfigError, match="musi zawierać mapowanie"):
        load_config()


def test_unknown_key_is_rejected(given_config_file: WriteConfigFile) -> None:
    """Literówka ma wysadzić start, a nie zostać po cichu zignorowana."""
    given_config_file(CONFIG_WITH_UNKNOWN_KEY_YAML)

    with pytest.raises(ConfigError) as caught:
        load_config()

    assert "service.log_levl" in str(caught.value)


def test_real_backend_requires_endpoint(given_config_file: WriteConfigFile) -> None:
    given_config_file(CONFIG_WITH_REAL_BACKEND_YAML)

    with pytest.raises(ConfigError, match="wymaga podania 'endpoint'"):
        load_config()


def test_error_message_lists_every_problem(given_config_file: WriteConfigFile) -> None:
    """Administrator ma zobaczyć wszystkie błędy naraz, nie poprawiać ich po jednym."""
    given_config_file(
        "service:\n  port: 99999\nelasticsearch:\n  url: nie-jest-urlem\n  alias: ''\n"
    )

    with pytest.raises(ConfigError) as caught:
        load_config()

    message = str(caught.value)
    assert "service.port" in message
    assert "elasticsearch.url" in message
    assert "elasticsearch.alias" in message


def test_config_is_immutable(given_config_file: WriteConfigFile) -> None:
    given_config_file(MINIMAL_CONFIG_YAML)
    config = load_config()

    with pytest.raises(ValidationError):
        config.service.port = 9999
