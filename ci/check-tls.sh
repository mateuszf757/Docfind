#!/usr/bin/env bash
# Warunek zakończenia Etapu 3: wymuszone odnowienie certyfikatu przechodzi bez
# ingerencji — proxy zaczyna podawać nowy certyfikat bez restartu i bez ani
# jednego nieudanego żądania w trakcie.
#
#   ci/check-tls.sh
#   DOCFIND_TLS_RENEW_PRODUCTION=1 ci/check-tls.sh   także na produkcyjnym Let's Encrypt
#
# Na produkcyjnym Let's Encrypt wymuszone odnowienie wymaga jawnej zgody:
# każde to nowy certyfikat, a limit to 5 identycznych na tydzień — po jego
# wyczerpaniu domena przez tydzień nie dostanie certyfikatu, także tego, który
# odnowiłby się sam przed wygaśnięciem. Odnowienia testuje się na stagingu.
#
# Po drodze sprawdza resztę wejścia: trasę podpiętą pod Gateway, TLS
# zweryfikowany względem CA, przekierowanie HTTP→HTTPS, identyfikator żądania
# docierający do backendu i polityki na podach proxy, których nie widać
# w `helm template`, bo tworzy je kontroler w trakcie działania.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

gateway_namespace="gateway"
gateway="docfind"
app_namespace="docfind"
route="docfind-api"
expected_context="k3d-docfind"
cluster_file="$repo_root/deploy/k3d/cluster.yaml"

# Poniżej tej liczby pętla nie biegła naprawdę i zero błędów nic nie znaczy.
MIN_REQUESTS=50

fail() {
  echo "NIESPEŁNIONE: $*" >&2
  exit 1
}

for tool in kubectl cmctl curl openssl python3; do
  command -v "$tool" >/dev/null || { echo "BŁĄD: brak $tool — uruchom przez ./bin/mise run cluster:tls" >&2; exit 2; }
done

context=$(kubectl config current-context)
[[ "$context" == "$expected_context" ]] \
  || fail "bieżący kontekst to '$context', oczekiwano '$expected_context'"

https_port=$(grep -oE '127\.0\.0\.1:[0-9]+:443' "$cluster_file" | cut -d: -f2)
http_port=$(grep -oE '127\.0\.0\.1:[0-9]+:80' "$cluster_file" | cut -d: -f2)
hostname=$(kubectl -n "$gateway_namespace" get gateway "$gateway" \
  -o jsonpath='{.spec.listeners[?(@.name=="https")].hostname}')
issuer=$(kubectl -n "$gateway_namespace" get gateway "$gateway" \
  -o jsonpath='{.metadata.annotations.cert-manager\.io/cluster-issuer}')
secret="$gateway-tls"

df_log "host $hostname, wydawca $issuer, HTTPS na 127.0.0.1:$https_port"

# --- trasa i certyfikat ----------------------------------------------------------
kubectl -n "$gateway_namespace" wait gateway/"$gateway" --for=condition=Programmed --timeout=60s >/dev/null
route_status=$(kubectl -n "$app_namespace" get httproute "$route" -o json | python3 -c '
import json, sys
parents = json.load(sys.stdin).get("status", {}).get("parents", [])
conditions = {c["type"]: c["status"] for p in parents for c in p.get("conditions", [])}
print(conditions.get("Accepted", "brak"), conditions.get("ResolvedRefs", "brak"))')
[[ "$route_status" == "True True" ]] \
  || fail "HTTPRoute $route nie jest przyjęta przez Gateway (Accepted ResolvedRefs: $route_status)"
df_log "HTTPRoute przyjęta przez Gateway"

kubectl -n "$gateway_namespace" wait certificate/"$secret" --for=condition=Ready --timeout=300s >/dev/null \
  || fail "certyfikat $secret nie jest gotowy — kubectl -n $gateway_namespace describe certificate $secret"

# Weryfikacja zawsze względem CA, nigdy -k. Wyjątek: Let's Encrypt staging
# podpisuje certyfikatem głównym, którego celowo nie ma w żadnym magazynie
# zaufania — tam weryfikacji łańcucha nie da się zrobić, więc sprawdzamy
# przynajmniej, że certyfikat naprawdę przyszedł ze stagingu.
work_dir=$(mktemp -d)
cleanup() {
  [[ -n "${loop_pid:-}" ]] && kill "$loop_pid" 2>/dev/null
  rm -rf "$work_dir"
}
trap cleanup EXIT

tls_args=()
case "$issuer" in
  docfind-internal-ca)
    kubectl -n cert-manager get secret docfind-root-ca -o jsonpath='{.data.ca\.crt}' \
      | base64 -d > "$work_dir/ca.crt"
    tls_args=(--cacert "$work_dir/ca.crt")
    ;;
  letsencrypt-staging)
    tls_args=(--insecure)
    ;;
  letsencrypt) ;;
  *) fail "nieznany wydawca $issuer" ;;
esac

curl_https() {
  curl -sS --max-time 3 --resolve "$hostname:$https_port:127.0.0.1" "${tls_args[@]}" "$@"
}

served_certificate() {
  openssl s_client -connect "127.0.0.1:$https_port" -servername "$hostname" </dev/null 2>/dev/null \
    | openssl x509 -noout -serial -issuer -enddate 2>/dev/null
}

# --- HTTPS, nagłówki, przekierowanie -------------------------------------------
headers=$(curl_https -o /dev/null -D - "https://$hostname:$https_port/version") \
  || fail "HTTPS do $hostname nie działa (weryfikacja TLS albo trasa)"
grep -qiE '^HTTP/[0-9.]+ 200' <<<"$headers" || fail "GET /version nie zwraca 200"

if [[ "$issuer" == "letsencrypt-staging" ]]; then
  # Najpierw całe wyjście do zmiennej: `openssl | grep -q` przy pipefail
  # losowo kończy się porażką, gdy grep zamknie potok przed openssl.
  staging_certificate=$(served_certificate)
  [[ "$staging_certificate" == *STAGING* ]] \
    || fail "wydawca to letsencrypt-staging, a podany certyfikat nie pochodzi ze stagingu"
  df_log "HTTPS: 200, certyfikat ze stagingu Let's Encrypt (łańcuch z założenia niezaufany)"
else
  df_log "HTTPS: 200, łańcuch certyfikatu zweryfikowany ($issuer)"
fi

request_id=$(awk 'tolower($1) == "x-request-id:" {print $2}' <<<"$headers" | tr -d '\r')
[[ "$request_id" =~ ^[0-9a-f-]{36}$ ]] \
  || fail "brak identyfikatora żądania z proxy w odpowiedzi API (x-request-id: '${request_id:-brak}')"
df_log "X-Request-Id nadany przez proxy dociera do backendu: $request_id"

redirect=$(curl -sS --max-time 3 --resolve "$hostname:$http_port:127.0.0.1" -o /dev/null \
  -w '%{http_code} %{redirect_url}' "http://$hostname:$http_port/version")
[[ "$redirect" == "301 https://$hostname:$https_port/version" ]] \
  || fail "HTTP nie przekierowuje na HTTPS (otrzymano: $redirect)"
df_log "HTTP → 301 → https://$hostname:$https_port"

# --- pody proxy: polityki, których nie widać w helm template -----------------------
proxy_selector="gateway.envoyproxy.io/owning-gateway-name=$gateway,gateway.envoyproxy.io/owning-gateway-namespace=$gateway_namespace"

# kubectl zwraca obiekt List. Polityka przyjmuje osobne dokumenty, a List bez
# rozpakowania przeszłaby ją bez słowa — nie ma w niej żadnego Deploymentu do
# sprawdzenia. Dlatego rozpakowanie i jawne sprawdzenie, że coś w ogóle jest.
kubectl -n envoy-gateway-system get deployment -l "$proxy_selector" -o json > "$work_dir/proxy.json"
proxy_deployments=$(python3 -c '
import json, sys
print(len(json.load(open(sys.argv[1]))["items"]))' "$work_dir/proxy.json")
(( proxy_deployments >= 1 )) || fail "nie znaleziono Deploymentu proxy dla Gateway $gateway"
python3 -c '
import json, sys
for item in json.load(open(sys.argv[1]))["items"]:
    print("---")
    print(json.dumps(item))' "$work_dir/proxy.json" \
  | (cd "$repo_root/services/api" && uv run --frozen python "$repo_root/ci/check_policy.py" proxy --require-digest) \
  || fail "pody proxy łamią polityki (decyzje 16 i 20)"
proxy_nodes=$(kubectl -n envoy-gateway-system get pods -l "$proxy_selector" \
  -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u | grep -c . || true)
(( proxy_nodes >= 2 )) || fail "pody proxy stoją na $proxy_nodes węźle — awaria węzła zabrałaby całe wejście"
df_log "pody proxy: obrazy z digestem, bez limitu CPU, na $proxy_nodes węzłach"

# --- wymuszone odnowienie pod ruchem ------------------------------------------------
if [[ "$issuer" == "letsencrypt" && "${DOCFIND_TLS_RENEW_PRODUCTION:-0}" != "1" ]]; then
  df_log "produkcyjny Let's Encrypt: odnowienie pod ruchem pominięte (limit 5 certyfikatów na tydzień)"
  df_log "sprawdzone na produkcji: zaufany łańcuch, trasa, przekierowanie, nagłówki, polityki proxy"
  exit 0
fi

# Przy ACME odnowienie to nowe wyzwanie DNS-01: rekord TXT w Cloudflare, jego
# propagacja do publicznych resolwerów i walidacja po stronie Let's Encrypt.
# Własne CA podpisuje od ręki.
renew_timeout=120
[[ "$issuer" == letsencrypt* ]] && renew_timeout=420

before=$(served_certificate)
revision_before=$(kubectl -n "$gateway_namespace" get certificate "$secret" -o jsonpath='{.status.revision}')
df_log "przed odnowieniem: $(grep serial <<<"$before"), rewizja $revision_before"

# Każde żądanie to nowe połączenie TLS, czyli nowy handshake — dokładnie ten
# moment, w którym proxy podaje certyfikat. Pętla na keep-alive testowałaby
# stary handshake i przeoczyłaby problem z podmianą.
(
  while true; do
    code=$(curl_https -o /dev/null -w '%{http_code}' "https://$hostname:$https_port/version" 2>/dev/null) || code="000"
    echo "$code"
    sleep 0.1
  done
) > "$work_dir/requests.log" &
loop_pid=$!
sleep 3

df_log "cmctl renew $gateway_namespace/$secret"
cmctl renew --namespace "$gateway_namespace" "$secret" >/dev/null

# Odnowienie zakończone: rewizja wzrosła i certyfikat znów jest gotowy.
for _ in $(seq 1 "$renew_timeout"); do
  revision=$(kubectl -n "$gateway_namespace" get certificate "$secret" -o jsonpath='{.status.revision}')
  ready=$(kubectl -n "$gateway_namespace" get certificate "$secret" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  (( revision > revision_before )) && [[ "$ready" == "True" ]] && break
  sleep 1
done
(( revision > revision_before )) \
  || fail "cert-manager nie odnowił certyfikatu w $renew_timeout s (rewizja $revision) — kubectl -n $gateway_namespace get challenges,orders"
df_log "cert-manager odnowił certyfikat (rewizja $revision)"

# Proxy podaje nowy certyfikat sam — Envoy Gateway śledzi Secret i przekazuje
# go proxy przez SDS, bez restartu podów.
after=""
for _ in $(seq 1 60); do
  after=$(served_certificate)
  [[ "$(grep serial <<<"$after")" != "$(grep serial <<<"$before")" ]] && break
  sleep 1
done
[[ "$(grep serial <<<"$after")" != "$(grep serial <<<"$before")" ]] \
  || fail "proxy nadal podaje stary certyfikat 60 s po odnowieniu"
df_log "proxy podaje nowy certyfikat: $(grep serial <<<"$after")"

sleep 3
kill "$loop_pid" 2>/dev/null
wait "$loop_pid" 2>/dev/null || true
loop_pid=""

total=$(grep -c . "$work_dir/requests.log" || true)
failed=$(grep -vc '^200$' "$work_dir/requests.log" || true)
df_log "żądań HTTPS w trakcie odnowienia: $total, nieudanych: $failed"

(( total >= MIN_REQUESTS )) || fail "tylko $total żądań — pętla nie biegła wystarczająco długo"
if (( failed > 0 )); then
  sort "$work_dir/requests.log" | uniq -c >&2
  fail "$failed z $total żądań nie powiodło się podczas odnowienia certyfikatu"
fi

restarts=$(kubectl -n envoy-gateway-system get pods -l "$proxy_selector" \
  -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}' \
  | awk '{s += $1} END {print s + 0}')
(( restarts == 0 )) || fail "kontenery proxy restartowały się ($restarts razy) — podmiana certyfikatu nie powinna wymagać restartu"

df_log "warunek zakończenia Etapu 3 spełniony: odnowienie certyfikatu bez ingerencji i bez utraty żądania"
