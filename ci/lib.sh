#!/usr/bin/env bash
# Wspólne funkcje skryptów CI.
#
# Jedyne źródło prawdy o tożsamości artefaktu. Każde miejsce, które pyta
# "co to za wersja i z jakiego commita", pyta tutaj — inaczej /version
# zacznie kłamać, a wtedy nie da się zdiagnozować, co stoi na klastrze.

set -euo pipefail

# Wersja z git describe. Bez tagów spada na 0.0.0-dev.<liczba commitów>+<sha>,
# żeby build działał od pierwszego dnia, a wersja i tak rosła monotonicznie.
df_version() {
  local described dirty=""
  # git describe --dirty działa tylko wtedy, gdy trafi w tag. Na ścieżce
  # zapasowej musimy oznaczyć brudne drzewo sami, inaczej obraz zbudowany
  # z niezacommitowanych zmian poda commit, z którego nie powstał.
  [[ -n "$(git status --porcelain)" ]] && dirty="-dirty"

  if described=$(git describe --tags --match 'v*' --dirty 2>/dev/null); then
    printf '%s' "${described#v}"
  else
    printf '0.0.0-dev.%s+%s%s' \
      "$(git rev-list --count HEAD)" \
      "$(git rev-parse --short HEAD)" \
      "$dirty"
  fi
}

df_commit() {
  git rev-parse HEAD
}

df_built_at() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

# Build wydania z brudnego drzewa jest odrzucany. Obraz zbudowany z
# niezacommitowanych zmian nie da się odtworzyć z commita, który podaje
# /version — czyli /version kłamie, a cała diagnostyka stoi na tym endpoincie.
df_require_clean_tree() {
  local dirty
  dirty=$(git status --porcelain)
  if [[ -n "$dirty" ]]; then
    echo "BŁĄD: drzewo robocze jest brudne — build wydania odrzucony." >&2
    echo "$dirty" >&2
    return 1
  fi
}

# Wersja semver dopuszcza '+' w metadanych builda, tag Dockera nie.
# Tożsamość w version.json zostaje pełna; sanityzujemy tylko etykietę obrazu.
df_docker_tag() {
  local v="${1:-$(df_version)}"
  v="${v//[^A-Za-z0-9._-]/-}"
  printf '%s' "${v:0:128}"
}

# Nazwa obrazu w GHCR. Repozytorium GitHuba jest małymi literami w ścieżce obrazu.
df_image_name() {
  local service="$1"
  local owner="${GITHUB_REPOSITORY_OWNER:-mateuszf757}"
  printf 'ghcr.io/%s/docfind-%s' "${owner,,}" "$service"
}

# Odczyt pojedynczego pola z obiektu JSON podanego na stdin.
# Python zamiast jq, bo jq nie jest gwarantowane ani na runnerze, ani na
# maszynie deweloperskiej — a Python jest, bo buduje ten projekt.
df_json_field() {
  local field="${1:?podaj nazwę pola}"
  python3 -c '
import json
import sys

field = sys.argv[1]
try:
    document = json.load(sys.stdin)
except json.JSONDecodeError as exc:
    sys.exit(f"nieprawidlowy JSON: {exc}")

if not isinstance(document, dict):
    sys.exit("oczekiwano obiektu JSON")

try:
    print(document[field])
except KeyError:
    sys.exit(f"brak pola {field!r}")
' "$field"
}

# Obraz musi mówić o sobie prawdę: version.json wypieczony w środku wskazuje
# ten commit, z którego obraz powstał.
df_verify_image_identity() {
  local image_ref="${1:?podaj referencję obrazu}"
  local expected_commit="${2:?podaj oczekiwany commit}"
  local reported actual

  reported=$(docker run --rm --entrypoint cat "$image_ref" /app/version.json) || {
    echo "BŁĄD: nie udało się odczytać /app/version.json z $image_ref" >&2
    return 1
  }
  printf '%s\n' "$reported"

  actual=$(printf '%s' "$reported" | df_json_field commit) || {
    echo "BŁĄD: version.json w obrazie jest nieczytelny" >&2
    return 1
  }

  if [[ "$actual" != "$expected_commit" ]]; then
    echo "BŁĄD: obraz deklaruje commit $actual, oczekiwano $expected_commit" >&2
    return 1
  fi

  df_log "version.json zgodny z $expected_commit"
}

df_log() {
  printf '==> %s\n' "$*"
}
