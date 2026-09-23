# Baza Alpine zamiast Debian slim: python:3.12-slim to ~196 MB samej bazy,
# przy venv ważącym 54 MB nie da się na niej zejść poniżej limitu 200 MB
# z Etapu 1. Koszt tej zamiany — musl zamiast glibc — jest zapisany
# w docs/DECYZJE.md.

FROM python:3.12-alpine AS builder

WORKDIR /build

# uv instaluje dokładnie wersje z uv.lock, więc obraz dostaje to, co
# przeszło testy. pip z pyproject.toml wziąłby najnowsze pasujące do
# ograniczeń ">=" i obraz mógłby różnić się od tego, co zweryfikowało CI.
COPY --from=ghcr.io/astral-sh/uv:0.12.17 /uv /usr/local/bin/uv

# Potrzebne tylko wtedy, gdy któraś zależność nie ma koła dla musl
# i musi się skompilować. Zostaje w warstwie budującej, nie w obrazie.
RUN apk add --no-cache build-base

ENV UV_PROJECT_ENVIRONMENT=/opt/venv \
    UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_NO_CACHE=1

COPY pyproject.toml uv.lock ./
COPY src ./src

# --no-dev pomija ruff i pytest; uv nie instaluje pipa do venv, więc nie ma
# czego usuwać po fakcie. Obraz bez pipa to zresztą obraz, w którym nie da
# się doinstalować pakietu niewidocznego w gicie.
RUN uv sync --frozen --no-dev --no-editable


FROM python:3.12-alpine AS runtime

ARG VERSION=unknown
ARG COMMIT=unknown
ARG BUILT_AT=unknown

LABEL org.opencontainers.image.title="docfind-api" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILT_AT}" \
      org.opencontainers.image.source="https://github.com/mateuszf757/Docfind"

# Obraz niesie własną tożsamość — nie da się go rozdzielić z informacją,
# z jakiego commita powstał.
RUN mkdir -p /app/config \
 && printf '{"version":"%s","commit":"%s","built_at":"%s"}\n' \
      "${VERSION}" "${COMMIT}" "${BUILT_AT}" > /app/version.json \
 && addgroup -S -g 10001 docfind \
 && adduser -S -u 10001 -G docfind docfind \
 && chown -R docfind:docfind /app

COPY --from=builder /opt/venv /opt/venv

ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    DOCFIND_VERSION_FILE=/app/version.json \
    DOCFIND_CONFIG=/app/config/app.yml

WORKDIR /app
USER 10001

EXPOSE 8000

# Świadomie bez HEALTHCHECK. Kubernetes ignoruje tę instrukcję i korzysta
# z livenessProbe/readinessProbe z manifestu (Etap 2), więc w docelowym
# środowisku nikt jej nie czyta. Kosztuje za to 0,24 s przy zamykaniu
# kontenera i start całego interpretera Pythona co 10 sekund — pod limitem
# CPU w klastrze to nie jest zerowy koszt. Patrz docs/DECYZJE.md.

# Forma exec: python dostaje SIGTERM bezpośrednio, bez pośrednictwa powłoki.
# Wejściem jest __main__, który waliduje konfigurację zanim wstanie serwer.
ENTRYPOINT ["python", "-m", "docfind_api"]
