"""Testy tożsamości builda.

Najważniejsze są te o braku i uszkodzeniu pliku: /version ma przyznać się
do niewiedzy, a nie zmyślić wersję. Endpoint, który kłamie o tym, co działa,
jest gorszy niż jego brak.

Wersja nie pochodzi z obrazu, tylko z wdrożenia (decyzja 40): ten sam digest
przechodzi przez staging i trafia na produkcję, a numer wydania nadaje mu się
dopiero po akceptacji.
"""

from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

from docfind_api.models import UNKNOWN, UNKNOWN_BUILD
from tests.conftest import SetDeployedVersion, WriteBuildInfo, WriteRawVersionFile
from tests.data import (
    BUILD_FILE,
    DEPLOYED_VERSION,
    LEGACY_BUILD_FILE,
    MALFORMED_VERSION_JSON,
    NON_OBJECT_VERSION_JSON,
    PARTIAL_BUILD,
    RELEASE_BUILD,
)


def test_reports_build_identity(
    client: TestClient,
    given_build_info: WriteBuildInfo,
    given_deployed_version: SetDeployedVersion,
) -> None:
    given_build_info(BUILD_FILE)
    given_deployed_version(DEPLOYED_VERSION)

    assert client.get("/version").json() == RELEASE_BUILD.model_dump()


def test_unknown_version_when_deployment_does_not_set_it(
    client: TestClient, given_build_info: WriteBuildInfo
) -> None:
    given_build_info(BUILD_FILE)

    body = client.get("/version").json()

    assert body["version"] == UNKNOWN
    assert body["commit"] == BUILD_FILE["commit"]


def test_blank_deployed_version_is_unknown(
    client: TestClient,
    given_build_info: WriteBuildInfo,
    given_deployed_version: SetDeployedVersion,
) -> None:
    given_build_info(BUILD_FILE)
    given_deployed_version("  ")

    assert client.get("/version").json()["version"] == UNKNOWN


def test_version_in_build_file_is_ignored(
    client: TestClient,
    given_build_info: WriteBuildInfo,
    given_deployed_version: SetDeployedVersion,
) -> None:
    # Obraz nie decyduje o wersji — inaczej obraz dla produkcji musiałby się
    # różnić od tego, który przeszedł staging.
    given_build_info(LEGACY_BUILD_FILE)
    given_deployed_version(DEPLOYED_VERSION)

    assert client.get("/version").json() == RELEASE_BUILD.model_dump()


def test_unknown_when_build_file_is_missing(
    client: TestClient, given_no_version_file: Path
) -> None:
    assert client.get("/version").json() == UNKNOWN_BUILD.model_dump()


def test_deployed_version_without_build_file(
    client: TestClient, given_no_version_file: Path, given_deployed_version: SetDeployedVersion
) -> None:
    given_deployed_version(DEPLOYED_VERSION)

    body = client.get("/version").json()

    assert body["version"] == DEPLOYED_VERSION
    assert body["commit"] == UNKNOWN
    assert body["source_date"] == UNKNOWN


def test_unknown_when_build_file_is_malformed(
    client: TestClient, given_raw_version_file: WriteRawVersionFile
) -> None:
    given_raw_version_file(MALFORMED_VERSION_JSON)

    assert client.get("/version").json() == UNKNOWN_BUILD.model_dump()


def test_unknown_when_build_file_is_not_an_object(
    client: TestClient, given_raw_version_file: WriteRawVersionFile
) -> None:
    given_raw_version_file(NON_OBJECT_VERSION_JSON)

    assert client.get("/version").json() == UNKNOWN_BUILD.model_dump()


def test_missing_fields_fall_back_to_unknown(
    client: TestClient, given_build_info: WriteBuildInfo
) -> None:
    given_build_info(PARTIAL_BUILD)

    body = client.get("/version").json()

    assert body["commit"] == PARTIAL_BUILD["commit"]
    assert body["source_date"] == UNKNOWN
