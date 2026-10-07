"""Identyfikator żądania nadany przez proxy, odsyłany w odpowiedzi.

Identyfikator nadaje Envoy na wejściu do klastra (ClientTrafficPolicy
z requestID: Generate) i zapisuje go w swoim logu dostępowym. Aplikacja
odsyła go w nagłówku odpowiedzi i trzyma w request.state, skąd wezmą go
jej własne logi. Dzięki temu jeden identyfikator łączy zgłoszenie klienta,
wiersz logu proxy i wiersz logu aplikacji — a z zewnątrz da się sprawdzić,
że nagłówek naprawdę dociera do backendu (`dft check tls`).

Aplikacja niczego nie generuje: identyfikator, którego nie ma w logu proxy,
niczego nie łączy. Żądanie bez nagłówka — na przykład sonda kubeleta albo
wywołanie z wnętrza klastra z pominięciem proxy — przechodzi bez niego.

Czyste ASGI, nie BaseHTTPMiddleware, z tego samego powodu co metryki:
to drugie buforuje odpowiedź i psuje strumieniowanie (Etap 7).
"""

from __future__ import annotations

import re
from typing import Final

from starlette.datastructures import MutableHeaders
from starlette.types import ASGIApp, Message, Receive, Scope, Send

HEADER: Final = "x-request-id"

VALID_REQUEST_ID: Final = re.compile(r"[A-Za-z0-9._-]{1,128}")
"""Dopuszczalny identyfikator — z zapasem na UUID od Envoya.

API jest osiągalne w klastrze także z pominięciem proxy, więc nagłówek może
przyjść od kogokolwiek. Wartość ze znakami nowej linii albo kilobajtami danych
nie może trafić ani do nagłówka odpowiedzi, ani do logu.
"""


def request_id_of(scope: Scope) -> str | None:
    for name, value in scope.get("headers", []):
        if name == HEADER.encode("latin-1"):
            candidate = value.decode("latin-1")
            return candidate if VALID_REQUEST_ID.fullmatch(candidate) else None
    return None


class RequestIdMiddleware:
    def __init__(self, app: ASGIApp) -> None:
        self.app = app

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        request_id = request_id_of(scope)
        if request_id is None:
            await self.app(scope, receive, send)
            return

        scope.setdefault("state", {})["request_id"] = request_id

        async def send_with_request_id(message: Message) -> None:
            if message["type"] == "http.response.start":
                MutableHeaders(scope=message)[HEADER] = request_id
            await send(message)

        await self.app(scope, receive, send_with_request_id)
