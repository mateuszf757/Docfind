#!/usr/bin/env bash
# Ustawienia repozytorium GitHuba jako kod.
#
#   ci/apply-repo-settings.sh
#
# Stosuje regułę gałęzi z .github/rulesets/main.json i włącza auto-merge.
# Idempotentny: istniejąca reguła o tej samej nazwie jest aktualizowana,
# a nie dublowana. Wymaga gh zalogowanego kontem z uprawnieniami admina.
#
# Reguła jest warunkiem bezpieczeństwa auto-merge łatek od Dependabota:
# `gh pr merge --auto` czeka tylko na sprawdzenia wymagane przez regułę,
# więc bez niej scaliłby PR natychmiast, bez żadnego CI.
#
# integration_id 15368 to aplikacja GitHub Actions. Bez niego status "test"
# albo "build" mógłby zgłosić dowolny inny integrator z dostępem do statusów
# i spełnić wymaganie w jej imieniu.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

command -v gh >/dev/null || { echo "BŁĄD: brak gh — https://cli.github.com" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "BŁĄD: gh nie jest zalogowany — gh auth login" >&2; exit 1; }

repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
ruleset_file="$repo_root/.github/rulesets/main.json"
ruleset_name=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$ruleset_file")

df_log "repozytorium: $repo"

# Auto-merge musi być włączony w ustawieniach repozytorium, inaczej
# `gh pr merge --auto` kończy się błędem.
gh api --method PATCH "repos/$repo" -F allow_auto_merge=true --silent
df_log "auto-merge włączony"

existing_id=$(gh api "repos/$repo/rulesets" --jq ".[] | select(.name == \"$ruleset_name\") | .id")
if [[ -n "$existing_id" ]]; then
  gh api --method PUT "repos/$repo/rulesets/$existing_id" --input "$ruleset_file" --silent
  df_log "reguła '$ruleset_name' zaktualizowana (id $existing_id)"
else
  gh api --method POST "repos/$repo/rulesets" --input "$ruleset_file" --silent
  df_log "reguła '$ruleset_name' utworzona"
fi
