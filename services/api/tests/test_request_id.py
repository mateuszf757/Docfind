"""Testy identyfikatora żądania.

Identyfikator nadaje proxy; aplikacja ma go odesłać bez zmian, także przy
błędach, i nie może przepuścić wartości spoza bezpiecznego formatu — API jest
osiągalne w klastrze także z pominięciem proxy.
"""

from __future__ import annotations

import pytest
from fastapi import status
from fastapi.testclient import TestClient

from docfind_api.request_id import HEADER
from tests.data import ENVOY_REQUEST_ID, SAMPLE_QUERY, UNSAFE_REQUEST_IDS


def test_echoes_request_id_from_proxy(client: TestClient) -> None:
    response = client.get("/search", params={"q": SAMPLE_QUERY}, headers={HEADER: ENVOY_REQUEST_ID})

    assert response.headers[HEADER] == ENVOY_REQUEST_ID


def test_echoes_request_id_on_errors(client: TestClient) -> None:
    """Identyfikator jest najcenniejszy właśnie przy błędzie — zgłoszenie klienta
    z kodem 4xx musi dać się powiązać z wierszem logu proxy."""
    missing = client.get("/nie-ma-takiej-trasy", headers={HEADER: ENVOY_REQUEST_ID})
    invalid = client.get("/search", params={"q": ""}, headers={HEADER: ENVOY_REQUEST_ID})

    assert missing.status_code == status.HTTP_404_NOT_FOUND
    assert missing.headers[HEADER] == ENVOY_REQUEST_ID
    assert invalid.status_code == status.HTTP_422_UNPROCESSABLE_CONTENT
    assert invalid.headers[HEADER] == ENVOY_REQUEST_ID


def test_does_not_generate_request_id(client: TestClient) -> None:
    """Identyfikator, którego nie ma w logu proxy, niczego nie łączy."""
    response = client.get("/search", params={"q": SAMPLE_QUERY})

    assert HEADER not in response.headers


@pytest.mark.parametrize("unsafe", UNSAFE_REQUEST_IDS)
def test_drops_unsafe_request_id(client: TestClient, unsafe: str) -> None:
    # Jako bajty: klient testowy odrzuca nagłówek tekstowy spoza ASCII jeszcze
    # przed wysłaniem, a prawdziwy klient wyśle surowe bajty i serwer je przyjmie.
    response = client.get(
        "/search", params={"q": SAMPLE_QUERY}, headers={HEADER.encode(): unsafe.encode("utf-8")}
    )

    assert response.status_code == status.HTTP_200_OK
    assert HEADER not in response.headers
