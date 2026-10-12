"""Tokeny dostępu (Authorization: Bearer) z sekretu.

Sekret jest czytany przy każdym sprawdzeniu, a nie raz przy starcie.
Kubernetes podmienia pliki Secreta w wolumenie (bez subPath) po rotacji,
a proces ma przyjąć nową wartość bez restartu — to warunek Etapu 4.
Zmienna środowiskowa z Secreta byłaby migawką z chwili startu kontenera.

W pliku może być kilka tokenów, po jednym na linię. Rotacja przebiega bez
przerwy dla klientów: najpierw nowy token obok starego, potem bez starego.
Token nigdy nie trafia do komunikatu błędu ani do logu.
"""

from __future__ import annotations

import hmac

from docfind_api.secrets import SecretNotFound, get_secret


class NoTokens(SecretNotFound):
    """Sekret jest, ale nie ma w nim żadnego tokenu."""


def accepted_tokens(secret_name: str) -> tuple[str, ...]:
    """Tokeny z sekretu: linie bez białych znaków na brzegach, bez pustych."""
    tokens = tuple(line.strip() for line in get_secret(secret_name).splitlines() if line.strip())
    if not tokens:
        raise NoTokens(f"sekret '{secret_name}' nie zawiera żadnego tokenu")
    return tokens


def bearer(authorization: str | None) -> str | None:
    """Token z nagłówka Authorization w schemacie Bearer albo None."""
    if not authorization:
        return None
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() != "bearer" or not token.strip():
        return None
    return token.strip()


def authorized(presented: str, accepted: tuple[str, ...]) -> bool:
    """Czy token jest na liście — porównanie w stałym czasie, bez wcześniejszego
    wyjścia: czas odpowiedzi nie mówi, czy ani który token prawie pasował."""
    candidate = presented.encode()
    result = False
    for token in accepted:
        result |= hmac.compare_digest(candidate, token.encode())
    return result
