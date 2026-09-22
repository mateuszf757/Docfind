# Etap 0 daje działający, samoopisujący się obraz.
# Etap 1 dokłada healthcheck, weryfikację obsługi SIGTERM i limit rozmiaru.

FROM python:3.12-slim AS builder

WORKDIR /build
ENV PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1

COPY pyproject.toml ./
COPY src ./src

RUN python -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir .


FROM python:3.12-slim AS runtime

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
RUN printf '{"version":"%s","commit":"%s","built_at":"%s"}\n' \
      "${VERSION}" "${COMMIT}" "${BUILT_AT}" > /app-version.json \
 && mkdir -p /app \
 && mv /app-version.json /app/version.json \
 && useradd --system --uid 10001 --no-create-home docfind \
 && chown -R docfind:docfind /app

COPY --from=builder /opt/venv /opt/venv

ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    DOCFIND_VERSION_FILE=/app/version.json

WORKDIR /app
USER 10001

EXPOSE 8000

# Forma exec: uvicorn dostaje SIGTERM bezpośrednio, bez pośrednictwa powłoki.
ENTRYPOINT ["uvicorn", "docfind_api.main:app", "--host", "0.0.0.0", "--port", "8000"]
