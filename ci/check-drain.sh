#!/usr/bin/env bash
# Warunek zakończenia Etapu 2: `kubectl drain` węzła z repliką API nie
# powoduje ani jednego nieudanego żądania.
#
#   ci/check-drain.sh
#   DRAIN_NODE=k3d-docfind-server-0 ci/check-drain.sh
#
# Pętla żądań biegnie wewnątrz klastra, na innym węźle niż drenowany, i woła
# Service po nazwie DNS — tak jak każdy klient w klastrze. Z hosta przez
# `kubectl port-forward` ten test byłby bezwartościowy: port-forward trzyma
# się jednego poda i zrywa się razem z nim, niezależnie od Service i PDB.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

namespace="docfind"
release="docfind"
deployment="$release-api"
selector="app.kubernetes.io/instance=$release,app.kubernetes.io/component=api"
probe_pod="drain-probe"
probe_image="$DF_CURL_IMAGE"
expected_context="k3d-docfind"

# Poniżej tej liczby pętla nie biegła naprawdę i zero błędów nic nie znaczy.
MIN_REQUESTS=100

fail() {
  echo "NIESPEŁNIONE: $*" >&2
  exit 1
}

# Skrypt drenuje węzeł. Na złym kontekście zrobiłby to na prawdziwym klastrze.
context=$(kubectl config current-context)
[[ "$context" == "$expected_context" ]] \
  || fail "bieżący kontekst to '$context', oczekiwano '$expected_context' — nie drenuję cudzego klastra"

# --- warunki wstępne ---------------------------------------------------------
kubectl -n "$namespace" rollout status "deployment/$deployment" --timeout=60s >/dev/null

mapfile -t api_nodes < <(
  kubectl -n "$namespace" get pods -l "$selector" --field-selector=status.phase=Running \
    -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}'
)
(( ${#api_nodes[@]} >= 2 )) || fail "działa ${#api_nodes[@]} replik API, potrzeba co najmniej 2"

distinct_nodes=$(printf '%s\n' "${api_nodes[@]}" | sort -u | wc -l)
(( distinct_nodes >= 2 )) \
  || fail "wszystkie repliki stoją na ${api_nodes[0]} — drain zabrałby je naraz; to błąd rozkładu podów"

# DRAIN_NODE pozwala wskazać węzeł — potrzebne, żeby odtworzyć konkretny
# układ podów przy diagnozie. Wskazany węzeł musi hostować replikę API,
# inaczej drain niczego nie sprawdza.
target="${DRAIN_NODE:-${api_nodes[0]}}"
[[ " ${api_nodes[*]} " == *" $target "* ]] \
  || fail "na węźle $target nie ma repliki API — drain niczego by nie sprawdził"
all_nodes=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
probe_node=$(grep -vxF "$target" <<<"$all_nodes" | head -1 || true)
[[ -n "$probe_node" ]] || fail "brak węzła innego niż $target dla pętli żądań"

df_log "drenowany węzeł: $target; pętla żądań na: $probe_node"

# Co jeszcze stoi na drenowanym węźle. Jeśli coś zawiedzie, najpierw trzeba
# wiedzieć, czy razem z API nie wyjechał na przykład jedyny pod CoreDNS.
df_log "pody na $target przed drainem:"
kubectl get pods -A --field-selector "spec.nodeName=$target" --no-headers \
  | awk '{printf "     %-14s %s\n", $1, $2}'

cleanup() {
  kubectl uncordon "$target" >/dev/null 2>&1 || true
  kubectl -n "$namespace" delete pod "$probe_pod" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- pętla żądań ---------------------------------------------------------------
kubectl -n "$namespace" delete pod "$probe_pod" --ignore-not-found --wait=true >/dev/null

# Każde żądanie to nowe połączenie: klient trzymający keep-alive do umierającego
# poda testowałby zachowanie klienta, a nie Service i endpointy.
#
# Poza kodem HTTP każde żądanie zapisuje kod wyjścia curl i czasy faz. Bez tego
# "000" nie mówi, co zawiodło: exit=6 albo zawieszone dns= to rozwiązywanie
# nazwy (CoreDNS), connect=0 przy rozwiązanej nazwie to brak endpointu.
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $probe_pod
  namespace: $namespace
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  nodeSelector:
    kubernetes.io/hostname: $probe_node
  containers:
    - name: probe
      image: $probe_image
      command:
        - sh
        - -c
        - |
          while true; do
            curl -s -o /dev/null --max-time 2 \
              -w '%{http_code} exit=%{exitcode} dns=%{time_namelookup} connect=%{time_connect}\n' \
              "http://$deployment.$namespace.svc.cluster.local/search?q=drain"
            sleep 0.05
          done
EOF

kubectl -n "$namespace" wait --for=condition=Ready "pod/$probe_pod" --timeout=90s >/dev/null
sleep 5
warmup_log=$(kubectl -n "$namespace" logs "$probe_pod")
grep -q '^200 ' <<<"$warmup_log" \
  || fail "pętla żądań nie dostaje odpowiedzi 200 jeszcze przed drainem — test nie ma punktu odniesienia"

# --- drain -------------------------------------------------------------------
drain_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
df_log "kubectl drain $target"
kubectl drain "$target" --ignore-daemonsets --delete-emptydir-data --timeout=180s >/dev/null

kubectl -n "$namespace" rollout status "deployment/$deployment" --timeout=120s >/dev/null
drain_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
df_log "drain zakończony, repliki odtworzone ($drain_started → $drain_finished)"

# Chwila po drainie, bo błędy potrafią pojawić się dopiero wtedy, gdy nowy
# pod dostaje pierwszy ruch.
sleep 5

# --- werdykt -----------------------------------------------------------------
responses=$(kubectl -n "$namespace" logs --timestamps "$probe_pod")
total=$(grep -c . <<<"$responses" || true)
failures=$(awk '$2 != "200"' <<<"$responses")
failed=$(grep -c . <<<"$failures" || true)

df_log "żądań: $total, nieudanych: $failed"
kubectl -n "$namespace" get pods -l "$selector" -o wide --no-headers \
  | awk '{printf "     %-40s %s\n", $1, $7}'

(( total >= MIN_REQUESTS )) || fail "tylko $total żądań — pętla nie biegła wystarczająco długo"

if (( failed > 0 )); then
  echo "" >&2
  echo "Nieudane żądania (czas, kod HTTP, kod wyjścia curl, czasy faz w s):" >&2
  head -20 <<<"$failures" >&2
  # curl ustawia time_connect=0, gdy do połączenia w ogóle nie doszło.
  dns_failures=$(awk '$4 ~ /^dns=/ && $5 == "connect=0.000000"' <<<"$failures" | grep -c . || true)
  echo "" >&2
  echo "Z tego bez nawiązanego połączenia (DNS albo brak trasy): $dns_failures" >&2
  fail "$failed z $total żądań nie powiodło się podczas drainu"
fi

df_log "warunek zakończenia Etapu 2 spełniony: drain bez utraty żądania"
