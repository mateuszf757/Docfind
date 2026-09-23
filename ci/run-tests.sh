#!/usr/bin/env bash
# Lint i testy jednostkowe. Uruchamiane identycznie lokalnie i w pipelinie —
# jeśli przechodzi u ciebie, przechodzi w CI, bo to ten sam skrypt.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

SHELLCHECK_IMAGE="koalaman/shellcheck:v0.11.0"

command -v uv >/dev/null || { echo "BŁĄD: brak uv w PATH (zainstaluj: https://astral.sh/uv)" >&2; exit 1; }

# Shellcheck z przypiętego obrazu, nie z systemu. Runner GitHuba ma wersję
# z apt (0.9.0), lokalnie bywa inna — a różne wersje zgłaszają różne uwagi,
# więc skrypt przechodzący u ciebie potrafił wywalić CI.
df_log "shellcheck $SHELLCHECK_IMAGE"
# Glob rozwija bash na hoście, więc musi to zrobić w korzeniu repozytorium —
# inaczej wywołanie spoza niego przekazałoby dosłowne "ci/*.sh".
(cd "$repo_root" && docker run --rm -v "$repo_root:/mnt:ro" -w /mnt "$SHELLCHECK_IMAGE" ci/*.sh)

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
