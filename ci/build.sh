#!/usr/bin/env bash
# Budowanie obrazu usługi z wersją wstrzykniętą z gita.
#
#   ci/build.sh api            build lokalny, brudne drzewo dozwolone
#   RELEASE=1 ci/build.sh api  build wydania: czyste drzewo, commit z tagiem vX.Y.Z
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

# Build wydania to build wydanej wersji: df_version daje samo X.Y.Z tylko na
# commicie z tagiem vX.Y.Z (decyzja 30). Commit bez tagu albo tag w innej
# postaci dałby obraz wydania z wersją deweloperską.
if [[ "${RELEASE:-0}" == "1" && ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "BŁĄD: build wydania wymaga commita z tagiem vX.Y.Z, a wersja to $version." >&2
  exit 1
fi

df_log "usługa=$service wersja=$version tag=$tag commit=${commit:0:12}"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

# SOURCE_DATE_EPOCH jako build arg BuildKit rozpoznaje sam: ustawia z niego
# znaczniki czasu w konfiguracji i historii obrazu. rewrite-timestamp=true
# przycina do niego także czasy modyfikacji plików w warstwach — bez tego
# każdy RUN zapisywałby pliki z bieżącą datą i warstwa różniłaby się co build.
build_args=(
  --build-arg "VERSION=$version"
  --build-arg "COMMIT=$commit"
  --build-arg "SOURCE_DATE=$source_date"
  --build-arg "SOURCE_DATE_EPOCH=$source_date_epoch"
)

# Tag z commitem tylko poza wydaniem. Commit z tagiem vX.Y.Z buduje się dwa
# razy — z gałęzi i z tagu — z różną wersją w version.json, a wspólny tag
# <sha12> przeskakiwałby wtedy w rejestrze z obrazu z main na obraz wydania.
tags=(--tag "$image:$tag")
[[ "${RELEASE:-0}" == "1" ]] || tags+=(--tag "$image:${commit:0:12}")

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

# Kopia lokalna, na której biegną sprawdzenia, zawsze bez atestacji
# pochodzenia. Atestacja opisuje przebieg budowania, więc z natury różni się
# między buildami i zmienia digest indeksu — każdy build wyglądałby jak nowa
# zawartość i wywoływał rollout w deploy-local.sh. Do rejestru atestacja
# trafia osobnym eksportem, niżej.
docker buildx build \
  "${build_args[@]}" \
  --provenance=false \
  --output "$output" \
  "${tags[@]}" \
  "${extra_flags[@]}" \
  "$context"

df_log "zbudowano $image:$tag"

# Kryterium zakończenia Etapu 0: to, co obraz mówi o sobie, zgadza się z gitem.
df_verify_image_identity "$image:$tag" "$commit"

if [[ "${PUSH:-0}" == "1" ]]; then
  # Konfiguracja obrazu, który właśnie przeszedł sprawdzenie — z nią porównamy
  # to, co wyląduje w rejestrze.
  verified_config=$(df_image_config_digest "$image:$tag")

  # Publikacja prosto z BuildKitu (eksporter image z push=true), z atestacją
  # pochodzenia. Wcześniej obraz szedł do rejestru przez `docker push` z
  # magazynu demona i atestacja ginęła po drodze: w GHCR leżał sam manifest,
  # choć decyzja 21 obiecywała provenance. Build bierze warstwy z pamięci
  # podręcznej kopii lokalnej; że to ta sama zawartość, sprawdza
  # df_verify_published_image na tym, co faktycznie jest w rejestrze.
  # mode=max zapisuje pełną definicję builda — build argi nie są sekretami.
  df_log "publikacja do $image"
  docker buildx build \
    "${build_args[@]}" \
    --provenance=mode=max \
    --output type=image,push=true,rewrite-timestamp=true,unpack=false \
    "${tags[@]}" \
    --metadata-file "$work_dir/push.json" \
    "$context"

  digest=$(df_json_field containerimage.digest < "$work_dir/push.json")
  df_verify_published_image "$image@$digest" "$verified_config"
  df_log "opublikowano $image@$digest"

  # Digest, nie tag, identyfikuje opublikowany obraz — tag można nadpisać.
  # Kolejne zadania i środowiska promują ten digest zamiast budować od nowa.
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf 'image=%s\ndigest=%s\n' "$image" "$digest" >> "$GITHUB_OUTPUT"
  fi
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    # Backticki to formatowanie Markdown w podsumowaniu, nie podstawienie polecenia.
    # shellcheck disable=SC2016
    printf '### Opublikowany obraz %s\n\n| Wersja | Obraz |\n|---|---|\n| %s | `%s@%s` |\n' \
      "$service" "$version" "$image" "$digest" >> "$GITHUB_STEP_SUMMARY"
  fi
fi
