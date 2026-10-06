#!/usr/bin/env bash
# Testy jednostkowe w obrazie na musl — ta sama baza i ta sama łatka Pythona
# co produkcja (etap test w services/<usługa>/Dockerfile).
#
#   ci/test-image.sh api
#
# run-tests.sh uruchamia testy na interpreterze hosta: glibc i koła manylinux.
# Obraz biegnie na musl, z kołami musllinux. Kod, który przechodzi na jednym,
# a pada na drugim, wychodzi tylko tutaj.
#
# Kody wyjścia: 0 — testy przeszły, 1 — testy nie przeszły, 2 — awaria
# budowania albo środowiska. Rozdzielone, bo brak sieci przy uv sync nie może
# wyglądać jak czerwony test.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

service="${1:-api}"
context="$repo_root/services/$service"

[[ -d "$context" ]] || { echo "BŁĄD: brak usługi '$service' w services/" >&2; exit 2; }
command -v docker >/dev/null || { echo "BŁĄD: brak docker w PATH" >&2; exit 2; }

log=$(mktemp)
trap 'rm -f "$log"' EXIT

# Ten sam SOURCE_DATE_EPOCH co w build.sh: etap builder przychodzi wtedy
# z pamięci podręcznej po buildzie obrazu, zamiast budować się drugi raz.
# Wynik testów też jest w pamięci podręcznej — te same wejścia dają ten sam
# wynik, więc bez zmian w kodzie pytest nie biegnie ponownie.
df_log "testy $service na obrazie (musl)"
status=0
docker buildx build \
  --target test \
  --build-context "config=$repo_root/deploy/config" \
  --build-arg "SOURCE_DATE_EPOCH=$("$DF_DFT" identity source-date-epoch)" \
  --output type=cacheonly \
  --progress plain \
  "$context" >"$log" 2>&1 || status=$?

# Linia podsumowania pytesta albo informacja, że wynik wziął się z cache.
summary=$(grep -oE '[0-9]+ (passed|failed)[^"]*' "$log" | tail -1 || true)

if (( status == 0 )); then
  df_log "testy na musl: ${summary:-bez zmian od poprzedniego biegu (wynik z pamięci podręcznej)}"
  exit 0
fi

tail -40 "$log" >&2
if grep -q 'python -m pytest.*did not complete successfully' "$log"; then
  echo "BŁĄD: testy nie przechodzą na musl${summary:+ ($summary)} — wyżej wynik pytesta" >&2
  exit 1
fi
echo "BŁĄD: etap test nie zbudował się (kod $status) — to awaria budowania, nie wynik testów" >&2
exit 2
