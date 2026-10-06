#!/usr/bin/env bash
# Wspólne funkcje skryptów CI.
#
# Jedyne źródło prawdy o tożsamości artefaktu. Każde miejsce, które pyta
# "co to za wersja i z jakiego commita", pyta tutaj — inaczej /version
# zacznie kłamać, a wtedy nie da się zdiagnozować, co stoi na klastrze.

set -euo pipefail

# --- Przypięte wersje ---------------------------------------------------------
#
# Binarki narzędzi (kubectl, k3d, helm, kubeconform, shellcheck, actionlint,
# zizmor, gh) są w mise.toml, a ich sumy dla każdej platformy w mise.lock —
# instaluje je ./bin/mise install z weryfikacją, tak jak wcześniej
# install-tools.sh. Tutaj zostaje to, czego mise nie obsługuje: wersja
# Kubernetesa, obrazy, chart i źródła, z których korzysta CI.
#
# Sumy i digesty są zapisane tutaj, a nie pobierane razem z plikiem. Suma
# ściągnięta z tego samego serwera chroni tylko przed uszkodzeniem
# w transferze, a nie przed podmianą. Każda z poniższych zgadzała się
# z sumą opublikowaną przez autorów w dniu przypięcia.
#
# Zmienne są używane przez skrypty, które źródłują ten plik.
# shellcheck disable=SC2034
{
  # Wersja klastra. kubectl z mise.toml musi być tą samą wersją (pilnuje
  # run-tests.sh), a kubeconform sprawdza manifesty względem schematu
  # dokładnie tej wersji Kubernetesa.
  DF_KUBERNETES_VERSION="1.36.4"
  DF_K3S_IMAGE="rancher/k3s:v1.36.4-k3s1@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657"

  # Charty komponentów platformy. Pobierane jako plik i weryfikowane sumą,
  # a nie instalowane wprost z repozytorium Helma — `helm install --repo`
  # zainstalowałby to, co repozytorium akurat serwuje pod tą wersją.
  DF_COREDNS_CHART_VERSION="1.47.1"
  DF_COREDNS_CHART_URL="https://github.com/coredns/helm/releases/download/coredns-${DF_COREDNS_CHART_VERSION}/coredns-${DF_COREDNS_CHART_VERSION}.tgz"
  DF_COREDNS_CHART_SHA256="1587165a85ec63dec4603e2889a8a6f5af9222a63893b8ecc254dfeb80c0e1e0"

  # cert-manager: suma zgodna z digestem w indeksie charts.jetstack.io.
  DF_CERT_MANAGER_CHART_VERSION="v1.21.2"
  DF_CERT_MANAGER_CHART_URL="https://charts.jetstack.io/charts/cert-manager-${DF_CERT_MANAGER_CHART_VERSION}.tgz"
  DF_CERT_MANAGER_CHART_SHA256="73a56e1728edd6c99f1f31082618c3259d279a76b7ebd3d4bdc5475c2442d34a"

  # Envoy Gateway publikuje chart wyłącznie jako artefakt OCI. Suma pliku to
  # digest warstwy charta w manifeście rejestru — sprawdzona przy przypięciu
  # niezależnie od `helm pull`, przez manifest pobrany wprost z rejestru.
  DF_ENVOY_GATEWAY_CHART_REF="oci://docker.io/envoyproxy/gateway-helm"
  DF_ENVOY_GATEWAY_CHART_VERSION="v1.9.2"
  DF_ENVOY_GATEWAY_CHART_SHA256="1079cad009e0885f6e10e5f712257d8e5fdaf911d2ceb3fa1b78632c8f29bbf9"

  # Obraz sondy uruchamianej w klastrze przez check-drain.sh. Digest indeksu
  # wieloarchitekturowego, nie manifestu dla amd64 — tag jest przesuwalny,
  # a przypięcie ma działać na każdej architekturze węzła.
  DF_CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"

  # BuildKit dla sterownika docker-container w CI (docker/setup-buildx-action).
  # Bez przypięcia akcja bierze przesuwalny tag moby/buildkit, a decyzja 21
  # obiecuje ten sam obraz z lokalnego BuildKitu i z CI — to zależy od wersji,
  # bo frontend Dockerfile jest wbudowany w BuildKit. Lokalny BuildKit
  # przychodzi z Dockera; check-reproducible.sh ostrzega, gdy wersje się różnią.
  DF_BUILDKIT_IMAGE="moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3"

  # Schematy dla kubeconform z konkretnego commita yannh/kubernetes-json-schema.
  # Domyślnie kubeconform pobiera je przy każdym biegu z gałęzi master — bez
  # sumy i z treścią, która może się zmienić między dwoma biegami tego samego
  # commita projektu. SHA commita adresuje treść, więc pliki pod nim są
  # niezmienne; sieć jest potrzebna przy pierwszym biegu, potem schematy leżą
  # w .cache/kubeconform.
  DF_KUBECONFORM_SCHEMA_COMMIT="c9452fcf5ef03628ab8b07e5b3a6b6f989e543bf"

  # Wydanie Alpine pod pływającym tagiem python:3.14-alpine w Dockerfile.
  # Dependabot odświeża digest tego tagu i takie odświeżenie jest scalane
  # automatycznie jak łatka (decyzja 22) — ale pod tym samym tagiem pojawia się
  # też nowe wydanie Alpine, czyli nowy musl i OpenSSL. check-image-base.sh
  # porównuje obraz z tą wartością: nowe wydanie Alpine daje czerwony build,
  # dopóki człowiek nie podbije jej tutaj, w tym samym PR-ze.
  DF_BASE_ALPINE="3.24"
}

# Kubeconfig projektu zamiast globalnego ~/.kube/config — ta sama ścieżka co
# [env] w mise.toml. Skrypty, które tworzą klaster i drenują węzły, nie mogą
# dziedziczyć kontekstu z powłoki: KUBECONFIG ustawiony na firmowy klaster albo
# kontekst przełączony ręcznie wskazałby im cudzy klaster. deploy-local.sh
# zapisuje tu dane dostępowe klastra prosto z k3d.
DF_KUBECONFIG="$(git rev-parse --show-toplevel)/.cache/kubeconfig"
export KUBECONFIG="$DF_KUBECONFIG"

# Wersja uv przypięta w Dockerfile (FROM ghcr.io/astral-sh/uv:X.Y.Z@sha256:…).
# To jedyne źródło prawdy o uv: przypięcie digestem aktualizuje Dependabot,
# CI instaluje tę samą wersję (ci.yml), a run-tests.sh ostrzega, gdy lokalny
# uv jest inny. Druga kopia wersji, której Dependabot nie widzi, rozjeżdżałaby
# się z pierwszą przy każdej jego aktualizacji.
df_uv_version() {
  local dockerfile pinned
  dockerfile="$(git rev-parse --show-toplevel)/services/api/Dockerfile"
  pinned=$(grep -oE '^FROM ghcr\.io/astral-sh/uv:[0-9]+\.[0-9]+\.[0-9]+@' "$dockerfile") || {
    echo "BŁĄD: nie znaleziono przypięcia uv w $dockerfile" >&2
    return 1
  }
  pinned=${pinned#FROM ghcr.io/astral-sh/uv:}
  printf '%s' "${pinned%@}"
}

# Wersja Pythona (major.minor), na której biegną testy: services/api/.python-version.
# Obraz bierze ją z tagu bazy w Dockerfile; spójność pilnują run-tests.sh
# (plik ↔ Dockerfile) i check-image-base.sh (plik ↔ zbudowany obraz).
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

# Znacznik czasu builda według konwencji reproducible-builds.org: czas ostatniego
# commita, a nie chwila budowania. Dzięki temu ten sam commit zbudowany dziś
# i za miesiąc daje bajt w bajt ten sam obraz — a zmiana czasu budowania nie
# wywołuje rolloutu, w którym nic się nie zmieniło.
#
# Brudne drzewo dostaje bieżący czas: jego zawartość i tak nie odpowiada
# żadnemu commitowi, więc nie ma czego odtwarzać, a wersja niesie "-dirty".
df_source_date_epoch() {
  if [[ -n "$(git status --porcelain)" ]]; then
    date -u +%s
  else
    git log -1 --format=%ct
  fi
}

df_source_date() {
  date -u -d "@${1:-$(df_source_date_epoch)}" +%Y-%m-%dT%H:%M:%SZ
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
