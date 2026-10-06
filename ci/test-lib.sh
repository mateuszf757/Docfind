#!/usr/bin/env bash
# Testy funkcji tożsamości artefaktu z ci/lib.sh: df_version i df_docker_tag.
#
#   ci/test-lib.sh
#
# Wersja z gita trafia do /version, etykiet OCI i tagów w rejestrze, a od
# decyzji 30 wyznacza ją tag wydania. Błąd w tej logice nie psuje żadnego testu
# aplikacji — wychodzi dopiero jako obraz z cudzą wersją. Każdy przypadek
# biegnie więc na prawdziwym repozytorium gita w katalogu tymczasowym.
#
# Kody wyjścia: 0 — wszystkie przypadki spełnione, 1 — któryś nie, 2 — awaria.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Repozytorium testowe odcięte od konfiguracji gita autora: globalne
# tag.gpgSign, commit.gpgSign albo core.abbrev zmieniłyby przebieg testu,
# a tożsamość commitów jest tu tylko atrapą.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid

repo_git() {
  git -C "$work" "$@" >/dev/null || { echo "BŁĄD: git $* nie powiódł się" >&2; exit 2; }
}

commit() {
  repo_git commit --quiet --allow-empty -m "$1"
}

head_sha() {
  git -C "$work" rev-parse --short HEAD
}

failures=0

expect_version() {
  local description="$1" expected="$2" actual
  if ! actual=$(cd "$work" && df_version); then
    echo "NIESPEŁNIONE: $description — df_version zakończył się błędem" >&2
    failures=$((failures + 1))
  elif [[ "$actual" != "$expected" ]]; then
    echo "NIESPEŁNIONE: $description — oczekiwano $expected, jest $actual" >&2
    failures=$((failures + 1))
  else
    df_log "$description: $actual"
  fi
}

expect_docker_tag() {
  local version="$1" expected="$2" actual
  actual=$(df_docker_tag "$version")
  if [[ "$actual" != "$expected" ]]; then
    echo "NIESPEŁNIONE: tag obrazu dla $version — oczekiwano $expected, jest $actual" >&2
    failures=$((failures + 1))
  else
    df_log "tag obrazu dla $version: $actual"
  fi
}

repo_git init --quiet --initial-branch=main
commit "pierwszy"
commit "drugi"
expect_version "bez tagu" "0.0.0-dev.2+$(head_sha)"

touch "$work/zmiana"
expect_version "bez tagu, brudne drzewo" "0.0.0-dev.2+$(head_sha)-dirty"
rm "$work/zmiana"

repo_git tag --annotate v0.3.0 -m "Etap 3"
expect_version "na tagu wydania" "0.3.0"

# Obraz z niezacommitowanych zmian nie może podać się za wydanie.
touch "$work/zmiana"
expect_version "na tagu, brudne drzewo" "0.3.1-dev.0+$(head_sha)-dirty"
rm "$work/zmiana"

# Za tagiem wersja przedpremierowa następnej łatki: w porządku SemVer powyżej
# 0.3.0, a surowe git describe (0.3.0-2-g…) stałoby poniżej.
commit "trzeci"
commit "czwarty"
expect_version "dwa commity za tagiem" "0.3.1-dev.2+$(head_sha)"

# Tag z sufiksem i tag z literówką bliżej niż wydanie: pominięte, a nie błąd —
# tagów v* nie da się usunąć, więc błąd zatrzymałby buildy na zawsze.
repo_git tag v0.4.0-rc.1
repo_git tag v1.2.3.4
commit "piąty"
expect_version "tagi spoza postaci vX.Y.Z pominięte" "0.3.1-dev.3+$(head_sha)"

repo_git tag v0.4.0
expect_version "tag lekki też wyznacza wydanie" "0.4.0"

expect_docker_tag "0.3.0" "0.3.0"
expect_docker_tag "0.3.1-dev.2+abc1234" "0.3.1-dev.2-abc1234"
expect_docker_tag "0.3.1-dev.0+abc1234-dirty" "0.3.1-dev.0-abc1234-dirty"

if (( failures > 0 )); then
  echo "" >&2
  echo "Testy ci/lib.sh: $failures niespełnionych." >&2
  exit 1
fi

df_log "testy ci/lib.sh: wszystkie przypadki spełnione"
