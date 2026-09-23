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

# --- chart Helma -------------------------------------------------------------
# Helm i kubeconform z przypiętych obrazów, z tego samego powodu co shellcheck.
chart="deploy/charts/docfind"
chart_values=(--set api.image.tag=lint)

helm_in_docker() {
  docker run --rm -v "$repo_root:/apps:ro" -w /apps "$DF_HELM_IMAGE" "$@"
}

df_log "helm lint"
helm_in_docker lint "$chart" "${chart_values[@]}"

# -strict odrzuca pola, których nie ma w schemacie: literówka w nazwie pola
# manifestu jest inaczej po cichu ignorowana przez API server.
df_log "kubeconform względem Kubernetesa $DF_KUBERNETES_VERSION"
helm_in_docker template docfind "$chart" "${chart_values[@]}" \
  | docker run --rm -i "$DF_KUBECONFORM_IMAGE" \
      -strict -summary -kubernetes-version "$DF_KUBERNETES_VERSION" -

# Konfiguracja z values.yaml trafia do ConfigMapy i jest walidowana dopiero
# przy starcie poda. Sprawdzamy ją tym samym modelem już tutaj — zły config
# ma zatrzymać pipeline, a nie skończyć jako CrashLoopBackOff na klastrze.
df_log "konfiguracja z charta przechodzi walidację modelu"
helm_in_docker template docfind "$chart" "${chart_values[@]}" \
    --show-only templates/api-configmap.yaml \
  | (cd "$repo_root/services/api" && uv run --frozen python -c '
import sys
import tempfile
from pathlib import Path

import yaml

from docfind_api.config import ConfigError, load_config

config_map = yaml.safe_load(sys.stdin)
with tempfile.TemporaryDirectory() as directory:
    path = Path(directory) / "app.yml"
    path.write_text(config_map["data"]["app.yml"], encoding="utf-8")
    try:
        load_config(path)
    except ConfigError as exc:
        sys.exit(f"BŁĄD: konfiguracja w charcie jest nieprawidłowa\n{exc}")
')

cd "$repo_root/services/api"

# Tryb projektowy uv: zależności biorą się z uv.lock, więc CI instaluje
# dokładnie te wersje co maszyna lokalna. --frozen pilnuje, żeby lockfile
# nie rozjechał się z pyproject.toml niezauważenie.
df_log "synchronizacja zależności z uv.lock"
uv sync --extra dev --frozen

df_log "ruff check"
uv run ruff check .

df_log "ruff format --check"
uv run ruff format --check .

df_log "pytest"
uv run pytest -q

# Schemat konfiguracji generowany z modelu musi być zacommitowany aktualny.
"$repo_root/ci/gen-schema.sh" --check

df_log "OK"
