#!/usr/bin/env bash
# Lint i testy jednostkowe. Uruchamiane identycznie lokalnie i w pipelinie —
# jeśli przechodzi u ciebie, przechodzi w CI, bo to ten sam skrypt.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

command -v uv >/dev/null || { echo "BŁĄD: brak uv w PATH (zainstaluj: https://astral.sh/uv)" >&2; exit 1; }

# Shellcheck z przypiętego obrazu (ci/lib.sh), nie z systemu. Runner GitHuba
# ma wersję z apt, lokalnie bywa inna — a różne wersje zgłaszają różne uwagi.
df_log "shellcheck $DF_SHELLCHECK_IMAGE"
# Glob rozwija bash na hoście, więc musi to zrobić w korzeniu repozytorium —
# inaczej wywołanie spoza niego przekazałoby dosłowne "ci/*.sh".
(cd "$repo_root" && docker run --rm -v "$repo_root:/mnt:ro" -w /mnt "$DF_SHELLCHECK_IMAGE" ci/*.sh)

# Workflowy GitHuba sprawdzane actionlintem: składnia, wyrażenia, nazwy
# uprawnień, a w blokach run także shellcheck. Błąd w workflowie wychodzi
# inaczej dopiero po wypchnięciu — a workflow auto-merge z uprawnieniami do
# scalania to ostatnie miejsce, w którym chce się to odkrywać w ten sposób.
df_log "actionlint $DF_ACTIONLINT_IMAGE"
(cd "$repo_root" && docker run --rm -v "$repo_root:/repo:ro" -w /repo "$DF_ACTIONLINT_IMAGE" -no-color)

# --- charty Helma ------------------------------------------------------------
# Helm i kubeconform z przypiętych obrazów, z tego samego powodu co shellcheck.
# Każdy chart jest renderowany raz, do pliku, i wszystkie sprawdzenia biegną
# na tym samym renderze — czyli na tym, co faktycznie trafiłoby na klaster.
render_dir=$(mktemp -d)
trap 'rm -rf "$render_dir"' EXIT

helm_in_docker() {
  docker run --rm -v "$repo_root:/apps:ro" -w /apps "$DF_HELM_IMAGE" "$@"
}

df_log "helm lint"
helm_in_docker lint deploy/charts/docfind --set api.image.tag=lint

helm_in_docker template docfind deploy/charts/docfind --set api.image.tag=lint \
  > "$render_dir/docfind.yaml"

coredns_chart=$(df_fetch_verified "$DF_COREDNS_CHART_URL" "$DF_COREDNS_CHART_SHA256")
helm_in_docker template coredns "${coredns_chart#"$repo_root"/}" --namespace kube-system \
    --values deploy/platform/coredns/values.yaml \
  > "$render_dir/coredns.yaml"

for rendered in "$render_dir"/*.yaml; do
  name=$(basename "$rendered" .yaml)

  # -strict odrzuca pola, których nie ma w schemacie: literówka w nazwie pola
  # manifestu jest inaczej po cichu ignorowana przez API server.
  df_log "kubeconform $name względem Kubernetesa $DF_KUBERNETES_VERSION"
  docker run --rm -i "$DF_KUBECONFORM_IMAGE" \
      -strict -summary -kubernetes-version "$DF_KUBERNETES_VERSION" - < "$rendered"

  # Poprawne względem schematu nie znaczy zgodne z decyzjami — domyślny
  # limit CPU z charta CoreDNS przeszedł kubeconform bez słowa.
  #
  # Digestu wymagamy od komponentów platformy, czyli obrazów z zewnątrz.
  # Nasz obraz w renderze testowym ma tag "lint"; jego tożsamość gwarantuje
  # build (version.json zgodny z commitem), a w dostawie do klienta wejdzie
  # digest z rejestru (Etap 10).
  policy_flags=()
  [[ "$name" == "docfind" ]] || policy_flags+=(--require-digest)
  df_log "polityki $name"
  (cd "$repo_root/services/api" && uv run --frozen python "$repo_root/ci/check_policy.py" "$name" "${policy_flags[@]}") \
    < "$rendered"
done

# Konfiguracja z values.yaml trafia do ConfigMapy i jest walidowana dopiero
# przy starcie poda. Sprawdzamy ją tym samym modelem już tutaj — zły config
# ma zatrzymać pipeline, a nie skończyć jako CrashLoopBackOff na klastrze.
df_log "konfiguracja z charta przechodzi walidację modelu"
(cd "$repo_root/services/api" && uv run --frozen python -c '
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

cd "$repo_root/services/api"

# Tryb projektowy uv: zależności biorą się z uv.lock, więc CI instaluje
# dokładnie te wersje co maszyna lokalna. --frozen pilnuje, żeby lockfile
# nie rozjechał się z pyproject.toml niezauważenie.
df_log "synchronizacja zależności z uv.lock"
uv sync --extra dev --frozen

df_log "ruff check"
uv run ruff check . "$repo_root/ci"

df_log "ruff format --check"
uv run ruff format --check . "$repo_root/ci"

df_log "pytest"
uv run pytest -q

# Schemat konfiguracji generowany z modelu musi być zacommitowany aktualny.
"$repo_root/ci/gen-schema.sh" --check

df_log "OK"
