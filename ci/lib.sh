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

df_log() {
  printf '==> %s\n' "$*"
}
