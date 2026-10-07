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

# Wersja Pythona (major.minor), na której biegną testy: services/api/.python-version.
# Obraz bierze ją z tagu bazy w Dockerfile; spójność pilnują
# `dft check versions` (plik ↔ Dockerfile) i check-image-base.sh
# (plik ↔ zbudowany obraz).
df_python_minor() {
  local file version
  file="$(git rev-parse --show-toplevel)/services/api/.python-version"
  version=$(<"$file")
  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+$ ]]; then
    echo "BŁĄD: $file ma zawierać wersję w postaci major.minor, a zawiera '$version'" >&2
    return 1
  fi
  printf '%s' "$version"
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

# Digest konfiguracji obrazu z lokalnego magazynu Dockera. Konfiguracja niesie
# digesty rozpakowanych warstw (diff_ids), więc ten sam digest konfiguracji to
# ta sama zawartość — niezależnie od kompresji i typu manifestu, którymi kopia
# lokalna różni się od tej w rejestrze. Z `docker save`, bo `docker image
# inspect .Id` znaczy co innego w magazynie klasycznym (konfiguracja) i w
# magazynie containerd (manifest), a BuildKit przy sterowniku docker nie podaje
# digestu konfiguracji w --metadata-file.
df_image_config_digest() {
  local ref="${1:?podaj referencję obrazu}" archive manifest
  archive=$(mktemp)
  if ! docker save -o "$archive" "$ref" || ! manifest=$(tar -xOf "$archive" manifest.json); then
    rm -f "$archive"
    echo "BŁĄD: nie udało się odczytać manifestu $ref z docker save" >&2
    return 2
  fi
  rm -f "$archive"
  python3 -c '
import json
import sys

config = json.loads(sys.argv[1])[0]["Config"]
# blobs/sha256/<hex> (Docker 25+) albo <hex>.json (starszy format)
name = config.rsplit("/", 1)[-1].removesuffix(".json")
print(f"sha256:{name}")
' "$manifest"
}

# Obraz w rejestrze to ten sprawdzony i ma atestację pochodzenia. Rozstrzyga
# rejestr, czyli system, który obraz przechowuje — nie metadane BuildKitu, które
# mówią, co BuildKit zamierzał wysłać. Decyzja 21 twierdziła, że opublikowane
# obrazy mają atestację, bo build jej nie wyłączał; rejestr trzymał sam
# manifest, bez indeksu i bez atestacji.
#
#   df_verify_published_image <obraz@digest> <digest konfiguracji sprawdzonego obrazu>
#
# Kody wyjścia: 0 — zgodny, 1 — inny obraz albo brak atestacji, 2 — rejestr
# nieosiągalny albo odpowiedź nieczytelna.
df_verify_published_image() {
  local ref="${1:?podaj obraz@digest}" expected_config="${2:?podaj digest konfiguracji}"
  local repository="${ref%@*}" index image_digest manifest config status=0

  index=$(docker buildx imagetools inspect --raw "$ref") || {
    echo "BŁĄD: nie udało się pobrać $ref z rejestru" >&2
    return 2
  }

  # Atestacja to osobny manifest w indeksie, wskazujący adnotacją na manifest
  # obrazu, którego dotyczy (konwencja BuildKitu: vnd.docker.reference.*).
  image_digest=$(python3 -c '
import json
import sys

ATTESTATION = "attestation-manifest"
index = json.loads(sys.argv[1])
manifests = index.get("manifests")
if not isinstance(manifests, list):
    media_type = index.get("mediaType", "?")
    print(f"NIESPEŁNIONE: w rejestrze jest {media_type}, a nie indeks — obraz bez atestacji pochodzenia", file=sys.stderr)
    sys.exit(1)

def annotation(descriptor, key):
    return descriptor.get("annotations", {}).get(f"vnd.docker.reference.{key}")

images = [m for m in manifests if annotation(m, "type") != ATTESTATION]
attested = {annotation(m, "digest") for m in manifests if annotation(m, "type") == ATTESTATION}
if len(images) != 1:
    print(f"NIESPEŁNIONE: indeks ma {len(images)} manifestów obrazu, oczekiwano jednego", file=sys.stderr)
    sys.exit(1)
if images[0]["digest"] not in attested:
    print("NIESPEŁNIONE: indeks nie ma manifestu atestacji dla obrazu", file=sys.stderr)
    sys.exit(1)
print(images[0]["digest"])
' "$index") || status=$?
  case "$status" in
    0) ;;
    1) return 1 ;;
    *) echo "BŁĄD: nieczytelny indeks $ref" >&2; return 2 ;;
  esac

  manifest=$(docker buildx imagetools inspect --raw "$repository@$image_digest") || {
    echo "BŁĄD: nie udało się pobrać manifestu $repository@$image_digest" >&2
    return 2
  }
  config=$(python3 -c 'import json, sys; print(json.loads(sys.argv[1])["config"]["digest"])' "$manifest") || {
    echo "BŁĄD: nieczytelny manifest $repository@$image_digest" >&2
    return 2
  }

  if [[ "$config" != "$expected_config" ]]; then
    echo "NIESPEŁNIONE: w rejestrze obraz z konfiguracją $config, a sprawdzony miał $expected_config" >&2
    return 1
  fi
  df_log "rejestr: atestacja pochodzenia obecna, konfiguracja zgodna z obrazem sprawdzonym ($config)"
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
