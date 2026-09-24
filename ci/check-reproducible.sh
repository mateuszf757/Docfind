#!/usr/bin/env bash
# Sprawdza, że build jest powtarzalny: build tego samego commita z pamięcią
# podręczną i build od zera dają identyczny obraz.
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

# Wymagania sprawdzane na starcie, a nie w połowie. Zadanie build w CI ma inne
# narzędzia niż zadanie test i niż maszyna lokalna — bez tej kontroli brak uv
# wyszedł dopiero po dwóch buildach, i to jako "build nie jest powtarzalny".
for tool in docker python3 tar; do
  command -v "$tool" >/dev/null || { echo "BŁĄD: brak $tool w PATH" >&2; exit 2; }
done

# Brudne drzewo dostaje bieżący czas zamiast czasu commita (df_source_date_epoch),
# więc z definicji nie jest powtarzalne — sprawdzanie go nic by nie dowiodło.
df_require_clean_tree

image="$(df_image_name "$service"):$(df_docker_tag "$(df_version)")"
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

# Pierwszy build korzysta z pamięci podręcznej, jaka akurat jest; drugi idzie
# od zera. Dwa buildy od zera przepuściły zależność od stanu pamięci podręcznej:
# warstwy z cache niosły czasy z wcześniejszych buildów, a build od zera — nie.
# W CI pamięć podręczna jest pusta i oba buildy są świeże; lokalnie ten wariant
# sprawdza dokładnie to, co robi deploy-local.sh.
for run in a b; do
  if [[ "$run" == "a" ]]; then
    df_log "build $run z pamięcią podręczną"
    no_cache=0
  else
    df_log "build $run od zera"
    no_cache=1
  fi
  NO_CACHE="$no_cache" "$repo_root/ci/build.sh" "$service" >"$work_dir/build-$run.log" 2>&1 || {
    tail -20 "$work_dir/build-$run.log" >&2
    echo "BŁĄD: build $run nie powiódł się" >&2
    exit 2
  }
  mkdir -p "$work_dir/$run"
  docker save "$image" | tar -x -C "$work_dir/$run"
done

# compare_oci.py korzysta wyłącznie z biblioteki standardowej, więc wystarcza
# systemowy python3 — bez uv i bez środowiska projektu.
#
# Kody wyjścia są rozdzielone: 1 to wynik (obrazy się różnią), każdy inny
# niezerowy to awaria samego porównania. Wcześniej oba kończyły się komunikatem
# "build nie jest powtarzalny" i brak narzędzia wyglądał jak różnica w warstwach.
df_log "porównanie manifestów, konfiguracji i warstw"
status=0
python3 "$repo_root/ci/compare_oci.py" "$work_dir/a" "$work_dir/b" || status=$?

case "$status" in
  0) df_log "build powtarzalny: build z pamięcią podręczną i build od zera dały identyczny obraz" ;;
  1) echo "BŁĄD: build nie jest powtarzalny — wyżej warstwy i pliki, które się różnią" >&2
     exit 1 ;;
  *) echo "BŁĄD: porównanie obrazów nie wykonało się (kod $status) — to awaria narzędzia, nie wynik" >&2
     exit 2 ;;
esac
