#!/usr/bin/env bash
# Sprawdza, że build jest powtarzalny: dwa buildy tego samego commita od zera,
# bez pamięci podręcznej, dają identyczny obraz.
#
#   ci/check-reproducible.sh api
#
# Powtarzalność oznacza, że obraz da się niezależnie odtworzyć z commita i że
# ta sama zawartość ma zawsze ten sam digest — więc zmiana digestu na klastrze
# zawsze oznacza zmianę zawartości, a nie tylko chwilę budowania.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

service="${1:-api}"

# Brudne drzewo dostaje bieżący czas zamiast czasu commita (df_source_date_epoch),
# więc z definicji nie jest powtarzalne — sprawdzanie go nic by nie dowiodło.
df_require_clean_tree

image="$(df_image_name "$service"):$(df_docker_tag "$(df_version)")"
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

for run in a b; do
  df_log "build $run od zera"
  NO_CACHE=1 "$repo_root/ci/build.sh" "$service" >"$work_dir/build-$run.log" 2>&1 || {
    tail -20 "$work_dir/build-$run.log" >&2
    echo "BŁĄD: build $run nie powiódł się" >&2
    exit 1
  }
  mkdir -p "$work_dir/$run"
  docker save "$image" | tar -x -C "$work_dir/$run"
done

df_log "porównanie manifestów, konfiguracji i warstw"
if (cd "$repo_root/services/api" && uv run --frozen python "$repo_root/ci/compare_oci.py" "$work_dir/a" "$work_dir/b"); then
  df_log "build powtarzalny: dwa buildy od zera dały identyczny obraz"
else
  echo "BŁĄD: build nie jest powtarzalny — wyżej warstwy i pliki, które się różnią" >&2
  exit 1
fi
