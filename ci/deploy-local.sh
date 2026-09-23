#!/usr/bin/env bash
# Wdrożenie na lokalny klaster k3d.
#
#   ci/deploy-local.sh           tworzy klaster, jeśli go nie ma, buduje obraz
#                                i instaluje chart
#   ci/deploy-local.sh --down    usuwa klaster
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

for tool in k3d kubectl helm docker; do
  command -v "$tool" >/dev/null || { echo "BŁĄD: brak $tool — uruchom ci/install-tools.sh" >&2; exit 1; }
done

if [[ "${1:-}" == "--down" ]]; then
  k3d cluster delete "$cluster"
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

# --- klaster -----------------------------------------------------------------
if k3d cluster get "$cluster" >/dev/null 2>&1; then
  df_log "klaster $cluster istnieje"
else
  preflight
  create_cluster
fi

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
df_log "helm upgrade --install $release"
helm upgrade --install "$release" "$repo_root/deploy/charts/docfind" \
  --namespace "$namespace" --create-namespace \
  --set "api.image.tag=$deploy_tag" \
  --wait --timeout 3m

kubectl -n "$namespace" rollout status "deployment/$release-api" --timeout=120s
kubectl -n "$namespace" get pods -o wide -l app.kubernetes.io/component=api

# To, co stoi na klastrze, musi się zgadzać z gitem — ta sama zasada co
# przy budowaniu obrazu, tylko sprawdzana na działającym podzie.
reported=$(kubectl -n "$namespace" exec "deployment/$release-api" -- \
  python -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8000/version").read().decode())')
df_log "/version na klastrze: $reported"
