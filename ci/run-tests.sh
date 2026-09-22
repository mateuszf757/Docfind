#!/usr/bin/env bash
# Lint i testy jednostkowe. Uruchamiane identycznie lokalnie i w pipelinie —
# jeśli przechodzi u ciebie, przechodzi w CI, bo to ten sam skrypt.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

command -v uv >/dev/null || { echo "BŁĄD: brak uv w PATH (zainstaluj: https://astral.sh/uv)" >&2; exit 1; }

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

df_log "OK"
