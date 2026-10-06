#!/usr/bin/env bash
# Lint, typy i testy jednostkowe. Uruchamiane identycznie lokalnie i w pipelinie —
# jeśli przechodzi u ciebie, przechodzi w CI, bo to ten sam skrypt.
#
#   ./bin/mise run test    całość: ten skrypt i bramki w Go (spójność wersji,
#                          kod narzędzi) — narzędzia z mise.lock
#   ci/run-tests.sh        tylko ta część, jeśli narzędzia są już w PATH
#
# W trakcie przenoszenia do Go (decyzja 23): spójność wersji i testy funkcji
# tożsamości są już w dft (`dft check versions`, `dft check go`).

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

# Wymagania sprawdzane na starcie i zgłaszane razem. Brak narzędzia w połowie
# biegu wyglądałby jak błąd w kodzie, a nie w środowisku.
missing=()
for tool in uv shellcheck actionlint zizmor helm kubeconform; do
  command -v "$tool" >/dev/null || missing+=("$tool")
done
if (( ${#missing[@]} > 0 )); then
  echo "BŁĄD: brak w PATH: ${missing[*]}" >&2
  echo "Uruchom przez mise, które instaluje je w wersjach z mise.lock: ./bin/mise run test" >&2
  exit 2
fi

# --- zależności Pythona ------------------------------------------------------
# --locked, nie --frozen. --frozen instaluje z uv.lock bez sprawdzenia, czy lock
# odpowiada pyproject.toml: zmiana zależności bez `uv lock` przechodziła przez CI
# bez słowa, a obraz budował się ze starego locka. --locked kończy się błędem,
# gdy lock jest nieaktualny. Dalej już --frozen, bo lock jest sprawdzony.
df_log "uv sync --locked"
(cd "$repo_root/services/api" && uv sync --locked)

# --- skrypty i workflowy -----------------------------------------------------
# Narzędzia w wersjach z mise.lock, nie z systemu — różne wersje zgłaszają
# różne uwagi; tak padł kiedyś pipeline na shellchecku 0.9.0 z apt.
#
# Glob rozwija bash, więc musi to zrobić w korzeniu repozytorium — inaczej
# wywołanie spoza niego przekazałoby dosłowne "ci/*.sh". ci/dft nie ma
# rozszerzenia, więc jest wymieniony osobno.
df_log "shellcheck"
(cd "$repo_root" && shellcheck ci/*.sh ci/dft)

# Workflowy GitHuba sprawdzane actionlintem: składnia, wyrażenia, nazwy
# uprawnień, a w blokach run także shellcheck. Błąd w workflowie wychodzi
# inaczej dopiero po wypchnięciu — a workflow auto-merge z uprawnieniami do
# scalania to ostatnie miejsce, w którym chce się to odkrywać w ten sposób.
df_log "actionlint"
(cd "$repo_root" && actionlint -no-color)

# actionlint sprawdza, czy workflow jest poprawny; zizmor — czy poprawny
# workflow nie jest groźny: wstrzyknięcia w wyrażeniach, nadmiarowe
# uprawnienia, zatruwanie cache, zbyt krótki cooldown Dependabota.
# --offline: bez tokenu i bez sieci, te same wyniki lokalnie i w CI.
df_log "zizmor"
(cd "$repo_root" && zizmor --offline --no-progress .github/)

# --- charty Helma ------------------------------------------------------------
# Każdy chart jest renderowany raz, do pliku, i wszystkie sprawdzenia biegną
# na tym samym renderze — czyli na tym, co faktycznie trafiłoby na klaster.
render_dir=$(mktemp -d)
trap 'rm -rf "$render_dir"' EXIT

chart="$repo_root/deploy/charts/docfind"

df_log "helm lint"
helm lint "$chart" --set api.image.tag=lint

# values.schema.json ma odrzucić literówkę — bez schematu Helm po cichu
# ignoruje nieznany klucz. Sprawdzamy odrzucenie, a nie samo istnienie pliku:
# schemat, który wszystko przepuszcza, też by „istniał".
if helm template docfind "$chart" --set api.image.tag=lint --set api.replica=3 >/dev/null 2>&1; then
  echo "BŁĄD: chart przyjął nieznany klucz api.replica — values.schema.json niczego nie pilnuje" >&2
  exit 1
fi

# Digest skrócony albo z literówką ma odpaść na schemacie, a nie przy pobieraniu
# obrazu na węźle; poprawny ma trafić do referencji obrazu w Deploymencie.
if helm template docfind "$chart" --set api.image.tag=lint --set api.image.digest=sha256:abc >/dev/null 2>&1; then
  echo "BŁĄD: chart przyjął skrócony digest api.image.digest — values.schema.json go nie pilnuje" >&2
  exit 1
fi
digest="sha256:$(printf '%064d' 0)"
digest_render=$(helm template docfind "$chart" --set api.image.tag=lint --set "api.image.digest=$digest")
if [[ "$digest_render" != *"docfind-api:lint@$digest\""* ]]; then
  echo "BŁĄD: api.image.digest nie trafił do referencji obrazu w Deploymencie" >&2
  exit 1
fi

helm template docfind "$chart" --set api.image.tag=lint > "$render_dir/docfind.yaml"

coredns_chart=$(df_fetch_verified "$DF_COREDNS_CHART_URL" "$DF_COREDNS_CHART_SHA256")
helm template coredns "$coredns_chart" --namespace kube-system \
    --values "$repo_root/deploy/platform/coredns/values.yaml" \
  > "$render_dir/coredns.yaml"

cert_manager_chart=$(df_fetch_verified "$DF_CERT_MANAGER_CHART_URL" "$DF_CERT_MANAGER_CHART_SHA256")
helm template cert-manager "$cert_manager_chart" --namespace cert-manager \
    --values "$repo_root/deploy/platform/cert-manager/values.yaml" \
  > "$render_dir/cert-manager.yaml"

envoy_gateway_chart=$(df_fetch_verified_oci_chart "$DF_ENVOY_GATEWAY_CHART_REF" \
  "$DF_ENVOY_GATEWAY_CHART_VERSION" "$DF_ENVOY_GATEWAY_CHART_SHA256")
helm template envoy-gateway "$envoy_gateway_chart" --namespace envoy-gateway-system \
    --values "$repo_root/deploy/platform/envoy-gateway/values.yaml" --include-crds \
  > "$render_dir/envoy-gateway.yaml"

# Chart platformy w obu wariantach: na własnym CA i z Let's Encrypt — inaczej
# szablony wydawców ACME nie byłyby renderowane, a więc ani sprawdzane.
platform_chart="$repo_root/deploy/charts/platform"
helm lint "$platform_chart"
if helm template platform "$platform_chart" --set gateway.hostnme=x >/dev/null 2>&1; then
  echo "BŁĄD: chart platformy przyjął nieznany klucz gateway.hostnme — values.schema.json niczego nie pilnuje" >&2
  exit 1
fi
helm template platform "$platform_chart" --namespace gateway > "$render_dir/platform.yaml"
helm template platform "$platform_chart" --namespace gateway \
    --set acme.email=ci@example.com --set acme.dnsZone=example.com \
    --set gateway.hostname=docfind.example.com --set gateway.issuer=letsencrypt-staging \
  > "$render_dir/platform-letsencrypt.yaml"

# Schematy zasobów z CRD — Gateway API, Envoy Gateway, cert-manager — z tych
# samych CRD, które instalują przypięte charty. Gotowe katalogi schematów CRD
# nie nadążają za wydaniami; walidacja względem starszej wersji przepuszczałaby
# pola, których API server nie przyjmie.
crd_schema_dir="$render_dir/crd-schemas"
cat "$render_dir/envoy-gateway.yaml" "$render_dir/cert-manager.yaml" \
  | (cd "$repo_root/services/api" && uv run --frozen python "$repo_root/ci/crd_schemas.py" "$crd_schema_dir")

# Schematy z commita przypiętego w ci/pins.env zamiast domyślnej gałęzi master.
# Pojedyncze cudzysłowy są celowe: {{ … }} rozwija kubeconform, nie powłoka.
# shellcheck disable=SC2016
schema_location='https://raw.githubusercontent.com/yannh/kubernetes-json-schema/'"$DF_KUBECONFORM_SCHEMA_COMMIT"'/{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json'
# shellcheck disable=SC2016
crd_schema_location="$crd_schema_dir"'/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json'
schema_cache="$repo_root/.cache/kubeconform"
mkdir -p "$schema_cache"

for rendered in "$render_dir"/*.yaml; do
  name=$(basename "$rendered" .yaml)

  # -strict odrzuca pola, których nie ma w schemacie: literówka w nazwie pola
  # manifestu jest inaczej po cichu ignorowana przez API server. Nigdy
  # -ignore-missing-schemas: przy CRD wyłączyłoby walidację bez słowa.
  df_log "kubeconform $name względem Kubernetesa $DF_KUBERNETES_VERSION"
  #
  # -skip CustomResourceDefinition: repozytorium schematów yannh nie ma schematu
  # dla samego rodzaju CRD (404 w każdym wariancie). Pominięty jest dokładnie
  # ten jeden rodzaj, nie wszystko bez schematu. Definicje CRD przychodzą
  # wyłącznie z chartów przypiętych sumą i nie piszemy ich sami — służą nam za
  # źródło schematów, a ich poprawność sprawdza API server przy instalacji.
  kubeconform -strict -summary -kubernetes-version "$DF_KUBERNETES_VERSION" \
    -schema-location "$schema_location" -schema-location "$crd_schema_location" \
    -skip CustomResourceDefinition -cache "$schema_cache" "$rendered"

  # Poprawne względem schematu nie znaczy zgodne z decyzjami — domyślny
  # limit CPU z charta CoreDNS przeszedł kubeconform bez słowa.
  #
  # Digestu wymagamy od komponentów platformy, czyli obrazów z zewnątrz.
  # Nasz obraz w renderze testowym ma tag "lint"; jego tożsamość gwarantuje
  # build (version.json zgodny z commitem), a w dostawie do klienta wejdzie
  # digest z rejestru (Etap 10).
  policy_flags=()
  [[ "$name" == "docfind" || "$name" == platform* ]] || policy_flags+=(--require-digest)
  df_log "polityki $name"
  (cd "$repo_root/services/api" && uv run --frozen python "$repo_root/ci/check_policy.py" "$name" "${policy_flags[@]}") \
    < "$rendered"
done

# Konfiguracja z values.yaml trafia do ConfigMapy i jest walidowana dopiero
# przy starcie poda. Sprawdzamy ją tym samym modelem już tutaj — zły config
# ma zatrzymać pipeline, a nie skończyć jako CrashLoopBackOff na klastrze.
# PYTHONPATH=src, bo projekt nie jest instalowany do środowiska — jak w obrazie.
df_log "konfiguracja z charta przechodzi walidację modelu"
(cd "$repo_root/services/api" && PYTHONPATH=src uv run --frozen python -c '
import sys
import tempfile
from pathlib import Path

import yaml

from docfind_api.config import ConfigError, load_config

config_map = next(
    doc for doc in yaml.safe_load_all(sys.stdin)
    if doc and doc["kind"] == "ConfigMap" and "app.yml" in doc.get("data", {})
)
with tempfile.TemporaryDirectory() as directory:
    path = Path(directory) / "app.yml"
    path.write_text(config_map["data"]["app.yml"], encoding="utf-8")
    try:
        load_config(path)
    except ConfigError as exc:
        sys.exit(f"BŁĄD: konfiguracja w charcie jest nieprawidłowa\n{exc}")
') < "$render_dir/docfind.yaml"

# --- Python ------------------------------------------------------------------
cd "$repo_root/services/api"

df_log "ruff check"
uv run --frozen ruff check . "$repo_root/ci"

df_log "ruff format --check"
uv run --frozen ruff format --check . "$repo_root/ci"

# Adnotacje typów bez sprawdzania to dokumentacja, która może kłamać.
df_log "mypy --strict"
uv run --frozen mypy src tests "$repo_root/ci/check_policy.py"

df_log "pytest"
uv run --frozen pytest -q

# Schemat konfiguracji generowany z modelu musi być zacommitowany aktualny.
"$repo_root/ci/gen-schema.sh" --check

df_log "OK"
