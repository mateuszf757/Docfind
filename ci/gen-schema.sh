#!/usr/bin/env bash
# Generuje deploy/config/app.schema.json z modelu Pydantic.
#
#   ci/gen-schema.sh          zapisuje schemat
#   ci/gen-schema.sh --check  sprawdza, czy zapisany schemat jest aktualny
#
# Schemat jest artefaktem generowanym, nie utrzymywanym ręcznie — inaczej
# rozjedzie się z walidacją i zacznie kłamać o tym, co aplikacja przyjmie.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

schema_path="$repo_root/deploy/config/app.schema.json"
# Pojedyncze cudzysłowy są celowe: "$schema" to klucz słownika w Pythonie,
# nie zmienna powłoki, i nie może zostać rozwinięty.
# shellcheck disable=SC2016
generated=$(cd "$repo_root/services/api" && uv run --frozen python -c '
import json
from docfind_api.config import AppConfig

schema = AppConfig.model_json_schema()
schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
schema["title"] = "Konfiguracja DOCFIND"
print(json.dumps(schema, indent=2, ensure_ascii=False))
')

if [[ "${1:-}" == "--check" ]]; then
  if ! diff -q <(printf '%s\n' "$generated") "$schema_path" >/dev/null 2>&1; then
    echo "BŁĄD: deploy/config/app.schema.json jest nieaktualny wobec modelu." >&2
    echo "Uruchom ci/gen-schema.sh i zacommituj wynik." >&2
    diff <(printf '%s\n' "$generated") "$schema_path" >&2 || true
    exit 1
  fi
  df_log "schemat aktualny"
  exit 0
fi

printf '%s\n' "$generated" > "$schema_path"
df_log "zapisano $schema_path"
