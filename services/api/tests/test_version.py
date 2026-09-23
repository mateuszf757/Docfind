"""Testy tożsamości builda.

Najważniejsze są te o braku i uszkodzeniu pliku: /version ma przyznać się
do niewiedzy, a nie zmyślić wersję. Endpoint, który kłamie o tym, co działa,
jest gorszy niż jego brak.
"""

from __future__ import annotations

from fastapi.testclient import TestClient

from docfind_api.models import UNKNOWN, UNKNOWN_BUILD
from tests.conftest import WriteBuildInfo, WriteRawVersionFile
from tests.data import MALFORMED_VERSION_JSON, NON_OBJECT_VERSION_JSON, PARTIAL_BUILD, RELEASE_BUILD


def test_reports_build_identity(client: TestClient, given_build_info: WriteBuildInfo) -> None:
    given_build_info(RELEASE_BUILD.model_dump())

    assert client.get("/version").json() == RELEASE_BUILD.model_dump()


def test_unknown_when_build_file_is_missing(client: TestClient, given_no_version_file) -> None:
    assert client.get("/version").json() == UNKNOWN_BUILD.model_dump()


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

    assert body["version"] == PARTIAL_BUILD["version"]
    assert body["commit"] == UNKNOWN
    assert body["source_date"] == UNKNOWN
