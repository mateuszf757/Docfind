# Baza Alpine zamiast Debian slim: python:3.12-slim to ~196 MB samej bazy,
# przy venv ważącym 54 MB nie da się na niej zejść poniżej limitu 200 MB
# z Etapu 1. Koszt tej zamiany — musl zamiast glibc — jest zapisany
# w docs/DECYZJE.md.
#
# Obrazy bazowe przypięte digestem: tag python:3.14-alpine przesuwa się przy
# każdym wydaniu łatki Pythona albo Alpine, więc ten sam commit budowany dwa
# razy dostawałby różne bazy. Aktualizacje digestów proponuje Dependabot
# (.github/dependabot.yml) — dlatego obrazy są wpisane wprost w FROM, a nie
# przez ARG: Dependabot nie rozwija zmiennych w FROM i takich przypięć nigdy
# by nie zaktualizował. Oba etapy muszą mieć tę samą bazę Pythona — bytecode
# kompilowany w builderze jest uruchamiany w runtime.

FROM ghcr.io/astral-sh/uv:0.12.19@sha256:04d046b13e60d6bcec73cbc5e1cad25d680dea90c8573340950a0ac2d1aef424 AS uv


FROM python:3.14-alpine@sha256:9e9fde4d32eedce0b661d9ab91e826b62dddf28e928c230ec55f1866cac66b01 AS builder

# Czas ostatniego commita (ci/lib.sh). Python, widząc tę zmienną, kompiluje
# pliki .pyc w trybie z sumą kontrolną zamiast z czasem modyfikacji źródła,
# więc bytecode też jest powtarzalny.
ARG SOURCE_DATE_EPOCH

WORKDIR /build

# uv instaluje dokładnie wersje z uv.lock, więc obraz dostaje to, co
# przeszło testy. pip z pyproject.toml wziąłby najnowsze pasujące do
# ograniczeń ">=" i obraz mógłby różnić się od tego, co zweryfikowało CI.
COPY --from=uv /uv /usr/local/bin/uv

# Bez `apk add build-base`: wszystkie zależności binarne mają gotowe koła dla
# musl, a kompilator pobierany z indeksu Alpine w chwili budowania to i tak
# zależność od stanu sieci, nie od commita. Jeśli któraś zależność straci
# koło dla musl, uv sync przerwie build zamiast kompilować po cichu.
ENV UV_PROJECT_ENVIRONMENT=/opt/venv \
    UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_NO_CACHE=1 \
    UV_PYTHON_DOWNLOADS=never \
    SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}

# Zależności, w osobnej warstwie: zmiana kodu aplikacji nie unieważnia jej,
# więc nie przeinstalowuje całego venv przy każdym buildzie.
#
# --frozen instaluje dokładnie uv.lock, bez rozwiązywania zależności od nowa.
# Nie sprawdza przy tym, czy lock odpowiada pyproject.toml — to robi
# `uv sync --locked` w ci/run-tests.sh, zanim obraz zostanie zbudowany.
# --no-dev pomija grupę dev (ruff, mypy, pytest). uv nie instaluje pipa do
# venv — obraz bez pipa to obraz, w którym nie da się doinstalować pakietu
# niewidocznego w gicie.
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project

# Kod aplikacji nie jest instalowany jako pakiet, tylko kopiowany jako pliki.
# Instalacja przez uv — wprost z katalogu i z koła — zapisywała w dist-info
# plik uv_cache.json z ctime źródła, którego nie da się ustawić z przestrzeni
# użytkownika; był to jedyny plik różniący dwa buildy tego samego commita
# (znaleziony przez ci/check-reproducible.sh). Pliki skopiowane i skompilowane
# do .pyc w trybie checked-hash są powtarzalne z samej konstrukcji.
COPY src /opt/app/src
RUN python -m compileall -q --invalidation-mode checked-hash /opt/app/src

# Czasy modyfikacji wszystkiego, co trafia do obrazu, ustawione na czas commita.
# Nie wystarcza rewrite-timestamp w BuildKit: on tylko przycina czasy nowsze od
# SOURCE_DATE_EPOCH, a starsze zostawia. Starsze pochodzą z kontekstu budowania
# i z warstw w pamięci podręcznej — czyli od stanu buildera, nie od commita.
# Tak dwa sterowniki BuildKit dawały różne warstwy przy identycznych plikach.
# Bytecode jest w trybie checked-hash, więc zmiana czasów go nie unieważnia.
RUN if [ -n "${SOURCE_DATE_EPOCH}" ]; then \
      find /opt/app /opt/venv -exec touch -h -d "@${SOURCE_DATE_EPOCH}" {} + ; \
    fi


# Testy jednostkowe na bazie produkcyjnej: musl i ta sama łatka Pythona co
# runtime. Zadanie test w CI i lokalny .venv biegną na glibc, z kołami
# manylinux; obraz biegnie na musl, z kołami musllinux — to inne binaria
# pydantic-core, uvloop i httptools. Etap nie trafia do obrazu: runtime kopiuje
# z buildera, nie stąd, a BuildKit buduje go tylko na żądanie
# (--target test, ci/test-image.sh).
#
# Układ katalogów jak w repozytorium, bo testy sięgają po
# deploy/config/app.yml.example ścieżką względną; ci/test-image.sh podaje ten
# katalog jako nazwany kontekst "config".
FROM builder AS test

WORKDIR /repo/services/api
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --group test --no-install-project
COPY src ./src
COPY tests ./tests
COPY --from=config app.yml.example /repo/deploy/config/app.yml.example
RUN /opt/venv/bin/python -m pytest -q -p no:cacheprovider


FROM python:3.14-alpine@sha256:9e9fde4d32eedce0b661d9ab91e826b62dddf28e928c230ec55f1866cac66b01 AS runtime

ARG VERSION=unknown
ARG COMMIT=unknown
ARG SOURCE_DATE=unknown

LABEL org.opencontainers.image.title="docfind-api" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${SOURCE_DATE}" \
      org.opencontainers.image.source="https://github.com/mateuszf757/Docfind"

# Obraz niesie własną tożsamość — nie da się go rozdzielić z informacją,
# z jakiego commita powstał.
#
# Użytkownik dopisany wprost do /etc/passwd i /etc/group zamiast przez
# adduser: busyboxowy adduser zapisuje w /etc/shadow dzisiejszą datę zmiany
# hasła, więc dwa buildy tego samego commita różniłyby się tą warstwą.
# Konto systemowe bez hasła i bez powłoki nie potrzebuje wpisu w shadow.
RUN mkdir -p /app/config \
 && printf '{"version":"%s","commit":"%s","source_date":"%s"}\n' \
      "${VERSION}" "${COMMIT}" "${SOURCE_DATE}" > /app/version.json \
 && echo 'docfind:x:10001:' >> /etc/group \
 && echo 'docfind:x:10001:10001:docfind:/app:/sbin/nologin' >> /etc/passwd

# Kod, zależności i version.json należą do roota — proces aplikacji może je
# czytać, ale nie zmienić. Aplikacja niczego nie zapisuje poza /tmp, więc
# nie ma powodu, żeby była właścicielem czegokolwiek w obrazie.
COPY --from=builder /opt/venv /opt/venv
COPY --from=builder /opt/app /opt/app

ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONPATH=/opt/app/src \
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
