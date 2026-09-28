#!/usr/bin/env bash
# Baza zbudowanego obrazu zgodna z zapisaną w repozytorium.
#
#   ci/check-image-base.sh api
#
# Dependabot odświeża digest pływającego tagu python:3.12-alpine i takie
# odświeżenie jest scalane automatycznie jak łatka (decyzja 22). Zwykle to
# łatka Pythona albo pakietów Alpine — ale pod tym samym tagiem pojawia się
# też nowe wydanie Alpine: nowy musl, nowy OpenSSL, a to nie jest łatka.
# Skrypt porównuje obraz z wydaniem Alpine z ci/lib.sh (DF_BASE_ALPINE)
# i z Pythonem z services/api/.python-version. Nowe wydanie zatrzymuje build,
# dopóki człowiek nie podbije wartości w tym samym PR-ze.
#
# W CI wersje bazy trafiają też do podsumowania zadania — przy przeglądzie
# PR-a od Dependabota widać, co naprawdę się zmieniło pod tym samym tagiem.
#
# Kody wyjścia: 0 — zgodna, 1 — baza inna niż zapisana, 2 — awaria sprawdzenia.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

service="${1:-api}"
image="$(df_image_name "$service"):$(df_docker_tag "$(df_version)")"

command -v docker >/dev/null || { echo "BŁĄD: brak docker w PATH" >&2; exit 2; }

# Jedno uruchomienie kontenera, wynik do zmiennej, parsowanie potem.
facts=$(docker run --rm --entrypoint /bin/sh "$image" -c '
  echo "alpine=$(cat /etc/alpine-release)"
  echo "python=$(python -c "import platform; print(platform.python_version())")"
  apk info -v 2>/dev/null | sed -n "s/^musl-\([0-9].*\)/musl=\1/p; s/^libssl3-\([0-9].*\)/openssl=\1/p"
') || { echo "BŁĄD: nie udało się odczytać bazy z $image — czy obraz jest zbudowany (ci/build.sh $service)?" >&2; exit 2; }

fact() {
  sed -n "s/^$1=//p" <<<"$facts"
}

alpine=$(fact alpine)
python=$(fact python)
[[ -n "$alpine" && -n "$python" ]] || { echo "BŁĄD: nieczytelne wersje bazy:" >&2; echo "$facts" >&2; exit 2; }

df_log "baza $image: Alpine $alpine, Python $python, musl $(fact musl), OpenSSL $(fact openssl)"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  printf '### Baza obrazu %s\n\n| Alpine | Python | musl | OpenSSL |\n|---|---|---|---|\n| %s | %s | %s | %s |\n' \
    "$service" "$alpine" "$python" "$(fact musl)" "$(fact openssl)" >> "$GITHUB_STEP_SUMMARY"
fi

failures=0

if [[ "${alpine%.*}" != "$DF_BASE_ALPINE" ]]; then
  echo "NIESPEŁNIONE: obraz stoi na Alpine $alpine, a zapisane jest $DF_BASE_ALPINE (DF_BASE_ALPINE w ci/lib.sh)." >&2
  echo "  Nowe wydanie Alpine przyszło pod tym samym tagiem bazy. Sprawdź zmiany musl i OpenSSL," >&2
  echo "  potem podbij DF_BASE_ALPINE w tym samym PR-ze." >&2
  failures=$((failures + 1))
fi

python_minor=$(df_python_minor)
if [[ "$(cut -d. -f1-2 <<<"$python")" != "$python_minor" ]]; then
  echo "NIESPEŁNIONE: obraz ma Pythona $python, a testy biegną na $python_minor (services/api/.python-version)." >&2
  failures=$((failures + 1))
fi

if (( failures > 0 )); then
  exit 1
fi
df_log "baza obrazu zgodna z zapisaną"
