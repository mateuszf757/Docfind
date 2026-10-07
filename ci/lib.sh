#!/usr/bin/env bash
# Wspólne funkcje skryptów CI — w trakcie przenoszenia do Go (decyzja 23).
#
# Tożsamość artefaktu (wersja, commit, czas źródeł, tag i nazwa obrazu) liczy
# już dft: tools/internal/identity, wołany jako ci/dft. To nadal jedno źródło
# prawdy — skrypty pytają ci/dft, zamiast pytać gita po swojemu, inaczej
# /version zacznie kłamać.

set -euo pipefail

df_repo_root=$(git rev-parse --show-toplevel)

# Przypięte wersje, obrazy i sumy — dane w ci/pins.env, wspólne dla Go,
# workflowów i tego pliku. set -a eksportuje je do programów wołanych ze
# skryptów, tak jak wcześniej zmienne z tego pliku.
# pins.env to dane, nie skrypt — shellcheck go nie śledzi (source=/dev/null),
# a format pilnuje dft (tools/internal/pins).
set -a
# shellcheck source=/dev/null
source "$df_repo_root/ci/pins.env"
set +a

# Wejście do narzędzi w Go. Skrypty biorą stąd tożsamość artefaktu:
#   version=$("$DF_DFT" version)    commit=$("$DF_DFT" identity commit)
# Używane w skryptach, które źródłują ten plik.
# shellcheck disable=SC2034
DF_DFT="$df_repo_root/ci/dft"

# Kubeconfig projektu zamiast globalnego ~/.kube/config — ta sama ścieżka co
# [env] w mise.toml. Skrypty, które tworzą klaster i drenują węzły, nie mogą
# dziedziczyć kontekstu z powłoki: KUBECONFIG ustawiony na firmowy klaster albo
# kontekst przełączony ręcznie wskazałby im cudzy klaster. deploy-local.sh
# zapisuje tu dane dostępowe klastra prosto z k3d.
DF_KUBECONFIG="$df_repo_root/.cache/kubeconfig"
export KUBECONFIG="$DF_KUBECONFIG"

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

# Pobiera plik do pamięci podręcznej w repozytorium i weryfikuje jego sumę
# przy każdym użyciu, nie tylko przy pobraniu — plik w pamięci podręcznej
# też może zostać podmieniony albo uszkodzony. Wypisuje ścieżkę do pliku.
# Pamięć podręczna leży w repozytorium (.cache/, poza gitem), żeby narzędzia
# uruchamiane w kontenerach widziały ją pod tym samym montowaniem co kod.
df_fetch_verified() {
  local url="$1" expected="$2" repo_root file actual
  repo_root=$(git rev-parse --show-toplevel)
  file="$repo_root/.cache/downloads/$(basename "$url")"
  mkdir -p "$(dirname "$file")"

  if [[ ! -f "$file" ]]; then
    # Plik tymczasowy unikalny dla wywołania. Dwa równoległe pobrania (mise
    # uruchamia zależności zadań równolegle) pisały wcześniej do tego samego
    # plik.part, a mv jednego z nich podmieniał plik w trakcie zapisu drugiego.
    local partial
    partial=$(mktemp "$file.part.XXXXXX")
    curl -fsSL -o "$partial" "$url" || { rm -f "$partial"; return 1; }
    mv "$partial" "$file"
  fi

  actual=$(sha256sum "$file" | cut -d' ' -f1)
  if [[ "$actual" != "$expected" ]]; then
    rm -f "$file"
    echo "BŁĄD: suma $(basename "$url") nie zgadza się z przypiętą (otrzymano $actual)." >&2
    return 1
  fi
  printf '%s' "$file"
}

# Chart z rejestru OCI do tej samej pamięci podręcznej co df_fetch_verified,
# z tą samą weryfikacją sumy przy każdym użyciu. `helm pull` sam nie weryfikuje
# niczego poza tym, co poda rejestr — suma przypięta w repozytorium jest
# niezależnym źródłem. Wypisuje ścieżkę do pliku.
df_fetch_verified_oci_chart() {
  local ref="$1" version="$2" expected="$3" repo_root file actual
  repo_root=$(git rev-parse --show-toplevel)
  file="$repo_root/.cache/downloads/$(basename "$ref")-$version.tgz"
  mkdir -p "$(dirname "$file")"

  if [[ ! -f "$file" ]]; then
    local pull_dir
    pull_dir=$(mktemp -d "$file.pull.XXXXXX")
    helm pull "$ref" --version "$version" --destination "$pull_dir" >/dev/null \
      || { rm -rf "$pull_dir"; return 1; }
    mv "$pull_dir/$(basename "$ref")-$version.tgz" "$file"
    rm -rf "$pull_dir"
  fi

  actual=$(sha256sum "$file" | cut -d' ' -f1)
  if [[ "$actual" != "$expected" ]]; then
    rm -f "$file"
    echo "BŁĄD: suma charta $ref $version nie zgadza się z przypiętą (otrzymano $actual)." >&2
    return 1
  fi
  printf '%s' "$file"
}

df_log() {
  printf '==> %s\n' "$*"
}
