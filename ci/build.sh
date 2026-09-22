#!/usr/bin/env bash
# Budowanie obrazu usługi z wersją wstrzykniętą z gita.
#
#   ci/build.sh api            build lokalny, brudne drzewo dozwolone
#   RELEASE=1 ci/build.sh api  build wydania, brudne drzewo odrzucone
#   PUSH=1 ci/build.sh api     publikacja do rejestru po weryfikacji
#
# Obraz jest samoopisujący się: version.json powstaje wewnątrz niego z build
# argów, więc nie da się go rozdzielić z tożsamością.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

service="${1:?podaj nazwę usługi, np. api}"
context="$repo_root/services/$service"

[[ -d "$context" ]] || { echo "BŁĄD: brak usługi '$service' w services/" >&2; exit 1; }

if [[ "${RELEASE:-0}" == "1" ]]; then
  df_require_clean_tree
fi

version=$(df_version)
tag=$(df_docker_tag "$version")
commit=$(df_commit)
built_at=$(df_built_at)
image=$(df_image_name "$service")

df_log "usługa=$service wersja=$version tag=$tag commit=${commit:0:12}"

docker build \
  --build-arg "VERSION=$version" \
  --build-arg "COMMIT=$commit" \
  --build-arg "BUILT_AT=$built_at" \
  --tag "$image:$tag" \
  --tag "$image:${commit:0:12}" \
  "$context"

df_log "zbudowano $image:$tag"

# Kryterium zakończenia Etapu 0: to, co obraz mówi o sobie, zgadza się z gitem.
df_verify_image_identity "$image:$tag" "$commit"

if [[ "${PUSH:-0}" == "1" ]]; then
  df_log "publikacja do $image"
  docker push "$image:$tag"
  docker push "$image:${commit:0:12}"
fi
