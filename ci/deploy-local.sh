#!/usr/bin/env bash
# Wdrożenie na lokalny klaster k3d.
#
#   ci/deploy-local.sh           tworzy klaster, jeśli go nie ma, buduje obraz
#                                i instaluje chart
#   ci/deploy-local.sh --down    usuwa klaster
#
# TLS na wejściu (Etap 3). Domyślnie własne CA i host docfind.internal.
# Let's Encrypt przez DNS-01 w Cloudflare — najpierw zawsze staging:
#
#   ci/set-dns-token.sh          token API Cloudflare do klastra, raz
#   DOCFIND_HOSTNAME=docfind.example.com DOCFIND_TLS_ISSUER=letsencrypt-staging \
#   DOCFIND_ACME_EMAIL=ja@example.com DOCFIND_ACME_ZONE=example.com \
#     ci/deploy-local.sh
#
# Obraz trafia do węzłów przez `k3d image import`, bez rejestru. Rejestr
# (GHCR) wchodzi do gry razem z ArgoCD na Etapie 5.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

cluster="docfind"
namespace="docfind"
release="docfind"
gateway_namespace="gateway"

hostname="${DOCFIND_HOSTNAME:-docfind.internal}"
issuer="${DOCFIND_TLS_ISSUER:-docfind-internal-ca}"
acme_email="${DOCFIND_ACME_EMAIL:-}"
acme_zone="${DOCFIND_ACME_ZONE:-}"

for tool in k3d kubectl helm docker; do
  command -v "$tool" >/dev/null || { echo "BŁĄD: brak $tool — uruchom przez ./bin/mise run cluster:up" >&2; exit 1; }
done

if [[ "${1:-}" == "--down" ]]; then
  k3d cluster delete "$cluster"
  rm -f "$KUBECONFIG"
  exit 0
fi

# --- warunki wstępne ---------------------------------------------------------
# k3d nie diagnozuje, dlaczego węzeł nie wstaje — czeka na linię "k3s is up
# and running", a po przekroczeniu czasu wycofuje klaster razem z logami węzła,
# czyli z jedynym dowodem przyczyny. Dlatego warunki, na których k3s się
# wykłada, sprawdzamy przed utworzeniem klastra, wszystkie naraz: część z nich
# wymaga restartu WSL, a zgłaszanie ich po jednym kosztowałoby restart na każdy.
preflight_failed=0

preflight_error() {
  echo "BŁĄD: $1" >&2
  while IFS= read -r line; do echo "    $line"; done <<<"$2" >&2
  echo >&2
  preflight_failed=1
}

preflight() {
  local cgroup_version controllers

  cgroup_version=$(docker info --format '{{.CgroupVersion}}')
  if [[ "$cgroup_version" != "2" ]]; then
    preflight_error \
      "Docker działa na cgroup v$cgroup_version, a kubelet $DF_KUBERNETES_VERSION wymaga cgroup v2." \
      "W %UserProfile%\\.wslconfig, sekcja [wsl2]: kernelCommandLine = cgroup_no_v1=all
Potem w PowerShellu: wsl --shutdown"
  else
    # Sprawdzamy z wnętrza kontenera, bo tylko tam widać, co faktycznie do
    # niego dociera — host może mieć cpuset, a kontener i tak go nie dostać.
    controllers=$(docker run --rm --entrypoint /bin/cat "$DF_K3S_IMAGE" /sys/fs/cgroup/cgroup.controllers)
    if [[ " $controllers " != *" cpuset "* ]]; then
      preflight_error \
        "kontener nie dostaje kontrolera cgroup cpuset (widzi: $controllers) — k3s kończy się 'failed to find cpuset cgroup (v2)'." \
        "Docker rootless: systemd deleguje do sesji użytkownika tylko cpu, memory i pids.
sudo mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\\nDelegate=cpu cpuset io memory pids\\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf
sudo systemctl daemon-reload
Potem w PowerShellu: wsl --shutdown"
    fi
  fi

  # Porty hosta czytane z cluster.yaml, żeby lista nie rozjechała się z definicją
  # klastra. Konflikt portu k3d zgłosiłby dopiero w połowie tworzenia klastra.
  local cluster_file="$repo_root/deploy/k3d/cluster.yaml" host_port busy_ports=()
  while read -r host_port; do
    if [[ -n "$(ss -ltnH "sport = :$host_port")" ]]; then
      busy_ports+=("$host_port")
    fi
  done < <(
    grep -oE '127\.0\.0\.1:[0-9]+:' "$cluster_file" | cut -d: -f2
    grep -oE 'hostPort: "[0-9]+"' "$cluster_file" | grep -oE '[0-9]+'
  )
  if (( ${#busy_ports[@]} > 0 )); then
    preflight_error \
      "porty hosta z deploy/k3d/cluster.yaml są zajęte: ${busy_ports[*]}." \
      "Sprawdź, kto słucha: ss -ltnp 'sport = :${busy_ports[0]}'"
  fi

  if (( preflight_failed )); then
    echo "Szczegóły: docs/WYMAGANIA.md" >&2
    exit 1
  fi
}

# --- tworzenie klastra z zabezpieczeniem logów --------------------------------
# Gdy węzeł nie wstanie w limicie czasu, k3d wycofuje klaster razem
# z kontenerami węzłów — a więc z jedynym dowodem przyczyny. Dlatego logi
# każdego węzła są przechwytywane od chwili powstania jego kontenera
# i zostają na dysku niezależnie od wyniku.
create_cluster() {
  local log_dir watcher_pid status=0 rootless_args=()

  # Docker rootless uruchamia węzły w przestrzeni nazw użytkownika, gdzie
  # kubelet nie może zapisywać globalnych sysctli jądra (vm/overcommit_memory,
  # kernel/panic) i kończy się "Failed to start ContainerManager". Bramka
  # KubeletInUserNamespace każe mu ten błąd pominąć. Dokładana tylko przy
  # rootless — na zwykłym Dockerze niepotrzebnie łagodziłaby kubeletowi
  # obsługę błędów, więc nie jest wpisana na stałe do cluster.yaml.
  if [[ "$(docker info --format '{{.SecurityOptions}}')" == *name=rootless* ]]; then
    rootless_args=(
      --k3s-arg "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*"
      --k3s-arg "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@agent:*"
    )
    df_log "Docker rootless: kubelet z bramką KubeletInUserNamespace"
  fi
  log_dir="${XDG_CACHE_HOME:-$HOME/.cache}/docfind/k3d-create-$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$log_dir"

  (
    followed=" "
    while true; do
      while read -r node; do
        [[ -z "$node" || "$followed" == *" $node "* ]] && continue
        docker logs -f "$node" >"$log_dir/$node.log" 2>&1 &
        followed+="$node "
      # Tylko uruchomione: `docker logs -f` na kontenerze w stanie Created
      # kończy się od razu i węzeł zostałby uznany za obserwowany z pustym logiem.
      done < <(docker ps --format '{{.Names}}' --filter "name=^k3d-$cluster-" --filter status=running)
      sleep 0.3
    done
  ) &
  watcher_pid=$!

  df_log "tworzenie klastra $cluster ($DF_K3S_IMAGE), logi węzłów: $log_dir"
  # Limit czasu, bo domyślnie k3d czeka na węzły bez końca — węzeł, który
  # nigdy nie wstanie, wygląda wtedy dokładnie jak węzeł, który wstaje powoli.
  k3d cluster create --config "$repo_root/deploy/k3d/cluster.yaml" \
    --image "$DF_K3S_IMAGE" --timeout 180s "${rootless_args[@]}" || status=$?

  pkill -P "$watcher_pid" 2>/dev/null || true
  kill "$watcher_pid" 2>/dev/null || true
  wait "$watcher_pid" 2>/dev/null || true

  if (( status != 0 )); then
    echo "BŁĄD: klaster nie powstał. Błędy z logów węzłów:" >&2
    local file
    for file in "$log_dir"/*.log; do
      [[ -e "$file" ]] || continue
      echo "--- $(basename "$file" .log)" >&2
      grep -E 'level=(fatal|error)|^E[0-9]{4} ' "$file" | grep -v 'connection to the server' | tail -8 >&2 || true
    done
    echo "Pełne logi: $log_dir" >&2
    exit "$status"
  fi
}

# --- limit inotify ---------------------------------------------------------------
# Przy Dockerze rootless wszystkie procesy we wszystkich kontenerach — węzły k3s,
# kubelety, containerd, każdy pod — działają na hoście jako jeden użytkownik
# i dzielą jego limit instancji inotify (domyślnie 128). Po jego wyczerpaniu
# Envoy nie dostaje inotify i kończy się SIGSEGV (`assert failure:
# inotify_fd_ >= 0`), a rolling update proxy podwaja na chwilę liczbę podów
# — właśnie wtedy limit pękał. Sprawdzane przy każdym wdrożeniu, bo limit
# zjada także wszystko inne w sesji użytkownika, nie tylko klaster.
MIN_INOTIFY_INSTANCES=512

check_inotify() {
  local limit used=0 fd
  limit=$(sysctl -n fs.inotify.max_user_instances)
  if (( limit < MIN_INOTIFY_INSTANCES )); then
    preflight_error \
      "fs.inotify.max_user_instances=$limit — za mało dla klastra w kontenerach (minimum $MIN_INOTIFY_INSTANCES); Envoy kończy się SIGSEGV przy wyczerpaniu." \
      "echo 'fs.inotify.max_user_instances=$MIN_INOTIFY_INSTANCES' | sudo tee /etc/sysctl.d/99-inotify.conf
sudo sysctl --system"
    return
  fi
  for fd in /proc/[0-9]*/fd/*; do
    [[ "$(readlink "$fd" 2>/dev/null)" == "anon_inode:inotify" ]] && used=$((used + 1))
  done
  df_log "inotify: $used z $limit instancji"
  (( used * 100 / limit < 80 )) \
    || echo "UWAGA: zużyte $used z $limit instancji inotify — rolling update może wyczerpać limit" >&2
}

# --- klaster -----------------------------------------------------------------
check_inotify
if (( preflight_failed )); then
  echo "Szczegóły: docs/WYMAGANIA.md" >&2
  exit 1
fi

if k3d cluster get "$cluster" >/dev/null 2>&1; then
  df_log "klaster $cluster istnieje"
else
  preflight
  create_cluster
fi

# Dane dostępowe prosto z k3d do kubeconfigu projektu (KUBECONFIG z ci/lib.sh),
# przy każdym uruchomieniu — także dla klastra utworzonego wcześniej, więc plik
# nigdy nie wskazuje na klaster, którego już nie ma. Globalny ~/.kube/config
# zostaje nietknięty (cluster.yaml: updateDefaultKubeconfig: false).
mkdir -p "$(dirname "$KUBECONFIG")"
(umask 077 && k3d kubeconfig get "$cluster" > "$KUBECONFIG")
kubectl config use-context "k3d-$cluster" >/dev/null
kubectl wait --for=condition=Ready nodes --all --timeout=120s >/dev/null
df_log "węzły gotowe: $(kubectl get nodes --no-headers | wc -l)"

# --- komponenty platformy ------------------------------------------------------
# CoreDNS przed aplikacją: bez niego żaden pod nie rozwiąże nazwy usługi.
coredns_chart=$(df_fetch_verified "$DF_COREDNS_CHART_URL" "$DF_COREDNS_CHART_SHA256")
df_log "CoreDNS: chart $DF_COREDNS_CHART_VERSION (suma zweryfikowana)"
helm upgrade --install coredns "$coredns_chart" \
  --namespace kube-system \
  --values "$repo_root/deploy/platform/coredns/values.yaml" \
  --wait --timeout 3m >/dev/null
kubectl -n kube-system rollout status deployment/coredns --timeout=120s >/dev/null
df_log "CoreDNS gotowy: $(kubectl -n kube-system get deploy coredns -o jsonpath='{.status.readyReplicas}') repliki"

# Kolejność wynika z zależności, nie z wygody. Envoy Gateway pierwszy, bo jego
# chart instaluje CRD Gateway API, a cert-manager z obsługą Gateway API
# wymaga ich przy starcie — bez nich nie czeka, tylko kończy się błędem
# i wpada w CrashLoopBackOff. Potem cert-manager, na końcu chart platformy,
# który tworzy zasoby obu.
envoy_gateway_chart=$(df_fetch_verified_oci_chart "$DF_ENVOY_GATEWAY_CHART_REF" \
  "$DF_ENVOY_GATEWAY_CHART_VERSION" "$DF_ENVOY_GATEWAY_CHART_SHA256")
df_log "Envoy Gateway: chart $DF_ENVOY_GATEWAY_CHART_VERSION (suma zweryfikowana)"
helm upgrade --install envoy-gateway "$envoy_gateway_chart" \
  --namespace envoy-gateway-system --create-namespace \
  --values "$repo_root/deploy/platform/envoy-gateway/values.yaml" \
  --wait --timeout 5m >/dev/null

# startupapicheck w charcie sprawia, że --wait kończy się dopiero wtedy, gdy
# webhook cert-managera naprawdę przyjmuje zapisy.
cert_manager_chart=$(df_fetch_verified "$DF_CERT_MANAGER_CHART_URL" "$DF_CERT_MANAGER_CHART_SHA256")
df_log "cert-manager: chart $DF_CERT_MANAGER_CHART_VERSION (suma zweryfikowana)"
helm upgrade --install cert-manager "$cert_manager_chart" \
  --namespace cert-manager --create-namespace \
  --values "$repo_root/deploy/platform/cert-manager/values.yaml" \
  --wait --timeout 5m >/dev/null

# Let's Encrypt wymaga tokenu Cloudflare w klastrze. Bez niego wydawca by
# powstał, a wyzwanie DNS-01 wisiałoby bez końca z błędem tylko w statusie
# Challenge — sprawdzamy to tutaj, z instrukcją.
if [[ "$issuer" == letsencrypt* ]] \
    && ! kubectl -n cert-manager get secret cloudflare-api-token >/dev/null 2>&1; then
  echo "BŁĄD: wydawca $issuer wymaga tokenu Cloudflare w klastrze — uruchom ci/set-dns-token.sh" >&2
  exit 1
fi

# Port przekierowania HTTP→HTTPS z mapowania load balancera w cluster.yaml —
# na hoście HTTPS słucha tam, a nie na 443 (decyzja 17).
https_port=$(grep -oE '127\.0\.0\.1:[0-9]+:443' "$repo_root/deploy/k3d/cluster.yaml" | cut -d: -f2)

df_log "platforma: Gateway dla $hostname, wydawca $issuer"
helm upgrade --install platform "$repo_root/deploy/charts/platform" \
  --namespace "$gateway_namespace" --create-namespace \
  --set "gateway.hostname=$hostname" \
  --set "gateway.issuer=$issuer" \
  --set "gateway.httpsRedirectPort=$https_port" \
  --set "acme.email=$acme_email" \
  --set "acme.dnsZone=$acme_zone" \
  --wait --timeout 3m >/dev/null
kubectl -n cert-manager wait certificate/docfind-root-ca --for=condition=Ready --timeout=120s >/dev/null
kubectl -n "$gateway_namespace" wait gateway/docfind --for=condition=Programmed --timeout=180s >/dev/null
df_log "Gateway zaprogramowany"

# --- obraz -------------------------------------------------------------------
"$repo_root/ci/build.sh" api

image=$(df_image_name api)
built_tag=$(df_docker_tag "$(df_version)")

# Tag wyliczony z zawartości obrazu, nie z wersji. Build z brudnego drzewa
# dostaje za każdym razem ten sam tag "...-dirty"; przy IfNotPresent
# i niezmienionej specyfikacji poda Helm nie zrobiłby rolloutu, a na
# klastrze dalej chodziłby stary kod pod nową nazwą.
image_id=$(docker image inspect "$image:$built_tag" --format '{{.Id}}')
deploy_tag="local-${image_id#sha256:}"
deploy_tag="${deploy_tag:0:18}"
docker tag "$image:$built_tag" "$image:$deploy_tag"

# Tryb direct wgrywa obraz prosto do containerd na każdym węźle. Domyślny
# tryb uruchamia pomocniczy kontener z zamontowanym gniazdem Dockera — to daje
# temu kontenerowi pełną kontrolę nad demonem, a przy rootless i tak zawodzi,
# bo k3d szuka gniazda pod /var/run/docker.sock zamiast w katalogu użytkownika.
df_log "import $image:$deploy_tag do węzłów"
k3d image import "$image:$deploy_tag" --cluster "$cluster" --mode direct >/dev/null

# --- chart -------------------------------------------------------------------
# Przestrzeń nazw z etykietą, która pozwala jej trasom podpiąć się pod
# listener HTTPS Gateway (gateway.routeNamespaceLabel w charcie platformy).
# Zgodę daje platforma, a nie aplikacja sama sobie — stąd nie w charcie docfind.
kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label namespace "$namespace" docfind.io/gateway-routes=allowed --overwrite >/dev/null

# Pod Security restricted (decyzja 31): API server odrzuca pod, który nie
# spełnia profilu — także taki, który nie przeszedł przez CI, jak sonda drainu.
# Wersja profilu przypięta do wersji klastra: "latest" zmieniałby reguły razem
# z aktualizacją Kubernetesa, bez przeglądu.
psa_version="v${DF_KUBERNETES_VERSION%.*}"
kubectl label namespace "$namespace" --overwrite \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version="$psa_version" \
  pod-security.kubernetes.io/warn=restricted \
  pod-security.kubernetes.io/warn-version="$psa_version" >/dev/null

df_log "helm upgrade --install $release"
helm upgrade --install "$release" "$repo_root/deploy/charts/docfind" \
  --namespace "$namespace" \
  --set "route.hostname=$hostname" \
  --set "api.image.tag=$deploy_tag" \
  --wait --timeout 3m

kubectl -n "$namespace" rollout status "deployment/$release-api" --timeout=120s
kubectl -n "$namespace" get pods -o wide -l app.kubernetes.io/component=api

# To, co stoi na klastrze, musi się zgadzać z gitem — ta sama zasada co przy
# budowaniu obrazu (df_verify_image_identity), sprawdzana na każdym podzie.
# Wcześniej /version jednego poda, wybranego przez `exec deployment/…`, było
# tylko wypisywane: inny commit na klastrze nie zatrzymywał niczego.
#
# Pody wygaszane po rolloutcie (z deletionTimestamp) przez preStop jeszcze
# odpowiadają starą wersją, więc są pomijane; liczba sprawdzonych podów musi
# się zgadzać z liczbą replik.
verify_deployed_identity() {
  local deployment="$1" container="$2" expected_image="$3" expected_version="$4" expected_commit="$5"
  local deployment_json selector replicas pods_json pods pod image reported version commit checked=0 failures=0

  # Wywołana w `|| { … }`, więc set -e w jej wnętrzu nie działa — każdy
  # odczyt, od którego zależy werdykt, jest sprawdzany jawnie.
  deployment_json=$(kubectl -n "$namespace" get deployment "$deployment" -o json) || return 1
  read -r selector replicas < <(python3 -c '
import json
import sys

spec = json.loads(sys.argv[1])["spec"]
labels = spec["selector"]["matchLabels"]
print(",".join(f"{key}={value}" for key, value in sorted(labels.items())), spec["replicas"])
' "$deployment_json")
  if [[ -z "$selector" || ! "$replicas" =~ ^[0-9]+$ ]]; then
    echo "BŁĄD: nie udało się odczytać selektora i liczby replik Deploymentu $deployment" >&2
    return 1
  fi

  pods_json=$(kubectl -n "$namespace" get pods -l "$selector" -o json) || return 1
  pods=$(python3 -c '
import json
import sys

container = sys.argv[2]
for pod in json.loads(sys.argv[1])["items"]:
    if pod["metadata"].get("deletionTimestamp"):
        continue
    images = [c["image"] for c in pod["spec"]["containers"] if c["name"] == container]
    print(pod["metadata"]["name"], images[0] if images else "-")
' "$pods_json" "$container")

  while read -r pod image; do
    [[ -n "$pod" ]] || continue
    checked=$((checked + 1))
    if [[ "$image" != "$expected_image" ]]; then
      echo "NIESPEŁNIONE: $pod uruchamia $image, oczekiwano $expected_image" >&2
      failures=$((failures + 1))
      continue
    fi
    # </dev/null: pętla czyta listę podów ze standardowego wejścia.
    if ! reported=$(kubectl -n "$namespace" exec "$pod" -c "$container" -- \
        python -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8000/version", timeout=5).read().decode())' \
        </dev/null); then
      echo "NIESPEŁNIONE: $pod nie odpowiada na /version" >&2
      failures=$((failures + 1))
      continue
    fi
    version=$(df_json_field version <<<"$reported")
    commit=$(df_json_field commit <<<"$reported")
    if [[ "$version" != "$expected_version" || "$commit" != "$expected_commit" ]]; then
      echo "NIESPEŁNIONE: $pod raportuje $version ($commit), oczekiwano $expected_version ($expected_commit)" >&2
      failures=$((failures + 1))
    else
      df_log "$pod: /version $version, commit ${commit:0:12}"
    fi
  done <<<"$pods"

  if (( checked != replicas )); then
    echo "NIESPEŁNIONE: sprawdzone pody: $checked, a Deployment $deployment ma $replicas replik" >&2
    failures=$((failures + 1))
  fi
  (( failures == 0 ))
}

verify_deployed_identity "$release-api" api "$image:$deploy_tag" "$(df_version)" "$(df_commit)" || {
  echo "BŁĄD: na klastrze działa coś innego niż obraz zbudowany z tego drzewa" >&2
  exit 1
}
