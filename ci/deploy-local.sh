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
# wykłada, sprawdzamy przed utworzeniem klastra, wszystkie naraz: każdy z nich
# wymaga restartu WSL, a zgłaszanie ich po jednym kosztowałoby restart na każdy.
preflight_failed=0

preflight_error() {
  echo "BŁĄD: $1" >&2
  while IFS= read -r line; do echo "    $line"; done <<<"$2" >&2
  echo >&2
  preflight_failed=1
}

preflight() {
  local cgroup_version controllers port_start

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

  if docker info --format '{{.SecurityOptions}}' | grep -q 'name=rootless'; then
    port_start=$(sysctl -n net.ipv4.ip_unprivileged_port_start)
    if (( port_start > 80 )); then
      preflight_error \
        "Docker rootless nie otworzy portów 80 i 443 z deploy/k3d/cluster.yaml (ip_unprivileged_port_start=$port_start)." \
        "echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-rootless-ports.conf
sudo sysctl --system"
    fi
  fi

  if (( preflight_failed )); then
    echo "Szczegóły: docs/WYMAGANIA.md" >&2
    exit 1
  fi
}

# --- klaster -----------------------------------------------------------------
if k3d cluster get "$cluster" >/dev/null 2>&1; then
  df_log "klaster $cluster istnieje"
else
  preflight
  df_log "tworzenie klastra $cluster ($DF_K3S_IMAGE)"
  # Limit czasu, bo domyślnie k3d czeka na serwer bez końca — węzeł, który
  # nigdy nie wstanie, wygląda wtedy dokładnie jak węzeł, który wstaje powoli.
  k3d cluster create --config "$repo_root/deploy/k3d/cluster.yaml" \
    --image "$DF_K3S_IMAGE" --timeout 180s
fi

kubectl config use-context "k3d-$cluster" >/dev/null
kubectl wait --for=condition=Ready nodes --all --timeout=120s >/dev/null
df_log "węzły gotowe: $(kubectl get nodes --no-headers | wc -l)"

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

df_log "import $image:$deploy_tag do węzłów"
k3d image import "$image:$deploy_tag" --cluster "$cluster" >/dev/null

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
