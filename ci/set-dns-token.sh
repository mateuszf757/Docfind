#!/usr/bin/env bash
# Token API Cloudflare do klastra, dla wydawców Let's Encrypt (DNS-01).
#
#   ci/set-dns-token.sh                         pyta o token, bez echa
#   op read op://dev/cloudflare/token | ci/set-dns-token.sh   z menedżera haseł
#   DOCFIND_ACME_ZONE=example.com ci/set-dns-token.sh         sprawdza też strefę
#
# Token nie trafia do gita, do historii powłoki ani do argumentów procesów —
# te widzi każdy użytkownik systemu w `ps`. Do curla i kubectl przechodzi przez
# potok z wbudowanego printf, który nie jest osobnym procesem. W klastrze jest
# zwykłym Secretem; na Etapie 4 przejmie go Vault przez External Secrets
# Operator (decyzja 5).
#
# Token z najmniejszymi uprawnieniami (Cloudflare → My Profile → API Tokens):
#   Zone → DNS → Edit   i   Zone → Zone → Read,
#   Zone Resources → Include → Specific zone → <ta jedna strefa>.
# Token z dostępem do wszystkich stref pozwoliłby po wycieku przejąć każdą
# domenę na koncie.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

secret_name="cloudflare-api-token"
secret_namespace="cert-manager"
zone="${DOCFIND_ACME_ZONE:-}"

for tool in kubectl curl python3; do
  command -v "$tool" >/dev/null || { echo "BŁĄD: brak $tool — uruchom przez ./bin/mise exec" >&2; exit 2; }
done

if [[ -t 0 ]]; then
  read -rsp "Token API Cloudflare (nie będzie widoczny): " token
  echo
else
  IFS= read -r token || true
fi
[[ -n "$token" ]] || { echo "BŁĄD: pusty token" >&2; exit 1; }

# Zapytanie do API Cloudflare z tokenem w nagłówku podanym przez stdin
# (--config -), a nie w argumencie curla.
cloudflare() {
  printf 'header = "Authorization: Bearer %s"\n' "$token" \
    | curl -sS --max-time 15 --config - "https://api.cloudflare.com/client/v4/$1"
}

# Weryfikacja przed zapisem: zły albo wygasły token wyszedłby inaczej dopiero
# jako wyzwanie DNS-01 wiszące bez końca, z błędem widocznym tylko w statusie
# zasobu Challenge.
status=$(cloudflare "user/tokens/verify" | python3 -c '
import json, sys
response = json.load(sys.stdin)
print(response.get("result", {}).get("status", "") if response.get("success") else "")')
if [[ "$status" != "active" ]]; then
  echo "BŁĄD: Cloudflare nie potwierdził tokenu (status: '${status:-brak}')" >&2
  exit 1
fi
df_log "token aktywny"

if [[ -n "$zone" ]]; then
  zones=$(cloudflare "zones?name=$zone" | python3 -c '
import json, sys
response = json.load(sys.stdin)
print(len(response.get("result") or []) if response.get("success") else -1)')
  if [[ "$zones" != "1" ]]; then
    echo "BŁĄD: token nie widzi strefy $zone — brakuje Zone → Zone → Read albo strefa jest spoza zakresu tokenu" >&2
    exit 1
  fi
  df_log "token widzi strefę $zone"
fi

kubectl create namespace "$secret_namespace" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
printf '%s' "$token" \
  | kubectl create secret generic "$secret_name" --namespace "$secret_namespace" \
      --from-file=api-token=/dev/stdin --dry-run=client -o yaml \
  | kubectl apply -f - >/dev/null
unset token

df_log "Secret $secret_namespace/$secret_name zapisany"
