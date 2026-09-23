#!/usr/bin/env bash
# Wspólne funkcje skryptów CI.
#
# Jedyne źródło prawdy o tożsamości artefaktu. Każde miejsce, które pyta
# "co to za wersja i z jakiego commita", pyta tutaj — inaczej /version
# zacznie kłamać, a wtedy nie da się zdiagnozować, co stoi na klastrze.

set -euo pipefail

# --- Przypięte wersje narzędzi ----------------------------------------------
#
# Jedno miejsce dla wszystkich wersji, bo narzędzie w innej wersji lokalnie
# i w CI zgłasza inne błędy — tak padł pipeline na shellchecku 0.9.0 z apt.
# Wersja klastra i kubectl są te same, a kubeconform sprawdza manifesty
# względem schematu dokładnie tej wersji Kubernetesa.
#
# Sumy SHA-256 są zapisane tutaj, a nie pobierane razem z plikiem. Suma
# ściągnięta z tego samego serwera chroni tylko przed uszkodzeniem
# w transferze, a nie przed podmianą. Każda z poniższych zgadzała się
# z sumą opublikowaną przez autorów w dniu przypięcia.
#
# Zmienne są używane przez skrypty, które źródłują ten plik.
# shellcheck disable=SC2034
{
  DF_KUBERNETES_VERSION="1.36.4"
  DF_K3S_IMAGE="rancher/k3s:v1.36.4-k3s1"

  DF_KUBECTL_VERSION="v1.36.4"
  DF_KUBECTL_SHA256="8b8f088da2dab964f853b38464033b1be15ede2839eca751482357c45abdd05a"

  DF_K3D_VERSION="v5.9.0"
  DF_K3D_SHA256="06d8f25bc3a971c4eb29e0ff08429b180402db0f4dec838c9eac427e296800a0"

  DF_HELM_VERSION="4.3.0"
  DF_HELM_SHA256="86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb"

  DF_KUBECONFORM_VERSION="v0.8.0"
  DF_KUBECONFORM_SHA256="9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883"

  # Charty komponentów platformy. Pobierane jako plik i weryfikowane sumą,
  # a nie instalowane wprost z repozytorium Helma — `helm install --repo`
  # zainstalowałby to, co repozytorium akurat serwuje pod tą wersją.
  DF_COREDNS_CHART_VERSION="1.47.1"
  DF_COREDNS_CHART_URL="https://github.com/coredns/helm/releases/download/coredns-${DF_COREDNS_CHART_VERSION}/coredns-${DF_COREDNS_CHART_VERSION}.tgz"
  DF_COREDNS_CHART_SHA256="1587165a85ec63dec4603e2889a8a6f5af9222a63893b8ecc254dfeb80c0e1e0"

  # Obrazy narzędzi uruchamianych w run-tests.sh — dzięki nim CI nie
  # potrzebuje niczego instalować, a wersja jest ta sama co lokalnie.
  DF_SHELLCHECK_IMAGE="koalaman/shellcheck:v0.11.0"
  DF_HELM_IMAGE="alpine/helm:${DF_HELM_VERSION}"
  DF_KUBECONFORM_IMAGE="ghcr.io/yannh/kubeconform:${DF_KUBECONFORM_VERSION}"
}

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
    curl -fsSL -o "$file.part" "$url"
    mv "$file.part" "$file"
  fi

  actual=$(sha256sum "$file" | cut -d' ' -f1)
  if [[ "$actual" != "$expected" ]]; then
    rm -f "$file"
    echo "BŁĄD: suma $(basename "$url") nie zgadza się z przypiętą (otrzymano $actual)." >&2
    return 1
  fi
  printf '%s' "$file"
}

df_log() {
  printf '==> %s\n' "$*"
}
