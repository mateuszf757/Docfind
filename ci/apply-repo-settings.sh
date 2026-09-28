#!/usr/bin/env bash
# Ustawienia repozytorium GitHuba jako kod.
#
#   ./bin/mise exec -- ci/apply-repo-settings.sh
#
# Stosuje reguły z .github/rulesets/*.json, włącza auto-merge i wymusza
# przypinanie akcji pełnym SHA. Idempotentny: istniejąca reguła o tej samej
# nazwie jest aktualizowana, a nie dublowana. Wymaga gh zalogowanego kontem
# z uprawnieniami admina.
#
# Reguła gałęzi main jest warunkiem bezpieczeństwa auto-merge łatek od
# Dependabota: `gh pr merge --auto` czeka tylko na sprawdzenia wymagane przez
# regułę, więc bez niej scaliłby PR natychmiast, bez żadnego CI.
# integration_id 15368 to aplikacja GitHub Actions. Bez niego status "test"
# albo "build" mógłby zgłosić dowolny inny integrator z dostępem do statusów
# i spełnić wymaganie w jej imieniu.
#
# Reguła tagów v* blokuje ich przesuwanie i usuwanie. Wersja w /version
# pochodzi z git describe, więc przesunięty tag zmieniłby to, co raportuje
# przebudowa tego samego commita — a /version jest podstawą diagnostyki.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

command -v gh >/dev/null || { echo "BŁĄD: brak gh — uruchom przez ./bin/mise exec -- ci/apply-repo-settings.sh" >&2; exit 2; }
gh auth status >/dev/null 2>&1 || { echo "BŁĄD: gh nie jest zalogowany — gh auth login" >&2; exit 2; }

repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
df_log "repozytorium: $repo"

# Auto-merge musi być włączony w ustawieniach repozytorium, inaczej
# `gh pr merge --auto` kończy się błędem.
gh api --method PATCH "repos/$repo" -F allow_auto_merge=true --silent
df_log "auto-merge włączony"

# Przypinanie akcji pełnym SHA wymuszane przez GitHuba, a nie tylko przez
# dyscyplinę i przegląd: workflow z akcją przypiętą tagiem nie ruszy.
# Pozostałe pola polityki są przepisywane z bieżących ustawień, żeby ich
# nie zmienić przy okazji.
permissions=$(gh api "repos/$repo/actions/permissions")
enabled=$(df_json_field enabled <<<"$permissions")
allowed_actions=$(df_json_field allowed_actions <<<"$permissions")
gh api --method PUT "repos/$repo/actions/permissions" \
  -F "enabled=${enabled,,}" -f "allowed_actions=$allowed_actions" -F sha_pinning_required=true --silent
df_log "akcje: wymagane przypięcie pełnym SHA (allowed_actions=$allowed_actions)"

for ruleset_file in "$repo_root"/.github/rulesets/*.json; do
  ruleset_name=$(df_json_field name < "$ruleset_file")
  existing_id=$(gh api "repos/$repo/rulesets" --jq ".[] | select(.name == \"$ruleset_name\") | .id")
  if [[ -n "$existing_id" ]]; then
    gh api --method PUT "repos/$repo/rulesets/$existing_id" --input "$ruleset_file" --silent
    df_log "reguła '$ruleset_name' zaktualizowana (id $existing_id)"
  else
    gh api --method POST "repos/$repo/rulesets" --input "$ruleset_file" --silent
    df_log "reguła '$ruleset_name' utworzona"
  fi
done
