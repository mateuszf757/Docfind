#!/usr/bin/env bash
# Budowanie obrazu usługi z wersją wstrzykniętą z gita.
#
#   ci/build.sh api            build lokalny, brudne drzewo dozwolone
#   RELEASE=1 ci/build.sh api  build wydania, brudne drzewo odrzucone
#   PUSH=1 ci/build.sh api     publikacja do rejestru po weryfikacji
#   NO_CACHE=1 ci/build.sh api build od zera, bez pamięci podręcznej
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
source_date_epoch=$(df_source_date_epoch)
source_date=$(df_source_date "$source_date_epoch")
image=$(df_image_name "$service")

df_log "usługa=$service wersja=$version tag=$tag commit=${commit:0:12}"

# SOURCE_DATE_EPOCH jako build arg BuildKit rozpoznaje sam: ustawia z niego
# znaczniki czasu w konfiguracji i historii obrazu. rewrite-timestamp=true
# przycina do niego także czasy modyfikacji plików w warstwach — bez tego
# każdy RUN zapisywałby pliki z bieżącą datą i warstwa różniłaby się co build.
#
# Opcje eksportu zależą od sterownika BuildKit. Sterownik docker (lokalny
# demon z magazynem containerd) eksportuje prosto do demona i odrzuca
# rewrite-timestamp razem z domyślnym rozpakowaniem warstw — rozpakowanie
# nastąpi przy pierwszym uruchomieniu. Sterownik docker-container (CI,
# setup-buildx-action) nie ma dostępu do magazynu demona, więc obraz wraca
# jako archiwum i jest ładowany — dopiero wtedy df_verify_image_identity
# może go uruchomić.
# NO_CACHE=1 buduje od zera — używane przez check-reproducible.sh, bo build
# z pamięci podręcznej trywialnie daje ten sam wynik i niczego nie dowodzi.
extra_flags=()
[[ "${NO_CACHE:-0}" == "1" ]] && extra_flags+=(--no-cache)

# Atestacja pochodzenia (provenance) opisuje przebieg budowania, więc z natury
# różni się między buildami i zmienia digest indeksu obrazu. Dla obrazu
# publikowanego do rejestru jest wartościowa i zostaje. Dla lokalnego nie daje
# nic, a przez zmienny digest każdy build wyglądałby jak nowa zawartość
# i wywoływał rollout w deploy-local.sh.
[[ "${PUSH:-0}" == "1" ]] || extra_flags+=(--provenance=false)

# Pełne wyjście do zmiennej, a dopiero potem parsowanie. `docker buildx inspect
# | awk '... exit'` przy pipefail losowo przerywał build bez komunikatu: awk
# kończył się po pierwszym dopasowaniu, buildx dostawał SIGPIPE, a set -e
# kończył skrypt. Ten sam wzorzec poprawiony w pozostałych skryptach ci/.
builder_info=$(docker buildx inspect)
builder_driver=$(awk '/^Driver:/ && !seen {print $2; seen = 1}' <<<"$builder_info")
case "$builder_driver" in
  docker) output="type=image,rewrite-timestamp=true,unpack=false" ;;
  *)      output="type=docker,rewrite-timestamp=true" ;;
esac

docker buildx build \
  --build-arg "VERSION=$version" \
  --build-arg "COMMIT=$commit" \
  --build-arg "SOURCE_DATE=$source_date" \
  --build-arg "SOURCE_DATE_EPOCH=$source_date_epoch" \
  --output "$output" \
  --tag "$image:$tag" \
  --tag "$image:${commit:0:12}" \
  "${extra_flags[@]}" \
  "$context"

df_log "zbudowano $image:$tag"

# Kryterium zakończenia Etapu 0: to, co obraz mówi o sobie, zgadza się z gitem.
df_verify_image_identity "$image:$tag" "$commit"

if [[ "${PUSH:-0}" == "1" ]]; then
  df_log "publikacja do $image"
  docker push "$image:$tag"
  docker push "$image:${commit:0:12}"
fi
