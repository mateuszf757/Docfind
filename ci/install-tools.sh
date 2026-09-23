#!/usr/bin/env bash
# Instalacja narzędzi do pracy z klastrem w przypiętych wersjach.
#
#   ci/install-tools.sh                  instaluje do ~/.local/bin
#   DF_TOOLS_DIR=/opt/bin ci/install-tools.sh
#
# Bez sudo i bez menedżera pakietów systemu: wersja z apt jest taka, jaką
# akurat ma dystrybucja, a nie taka, jaką sprawdzono z tym klastrem.
# Każdy plik jest weryfikowany względem sumy zapisanej w ci/lib.sh zanim
# trafi do PATH — plik o niezgodnej sumie nie jest instalowany wcale.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

tools_dir="${DF_TOOLS_DIR:-$HOME/.local/bin}"
mkdir -p "$tools_dir"

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

verify() {
  local file="$1" expected="$2" actual
  actual=$(sha256sum "$file" | cut -d' ' -f1)
  if [[ "$actual" != "$expected" ]]; then
    echo "BŁĄD: suma $(basename "$file") nie zgadza się z przypiętą." >&2
    echo "  oczekiwana: $expected" >&2
    echo "  otrzymana:  $actual" >&2
    return 1
  fi
}

# Pomija instalację, gdy w katalogu docelowym stoi już właściwa wersja.
already_installed() {
  local binary="$1" expected_version="$2"
  [[ -x "$tools_dir/$binary" ]] || return 1
  local reported
  reported=$("$tools_dir/$binary" "${@:3}" 2>/dev/null) || return 1
  [[ "$reported" == *"$expected_version"* ]]
}

install_binary() {
  local name="$1" source_file="$2"
  install -m 0755 "$source_file" "$tools_dir/$name"
  df_log "zainstalowano $name -> $tools_dir/$name"
}

if already_installed kubectl "$DF_KUBECTL_VERSION" version --client; then
  df_log "kubectl $DF_KUBECTL_VERSION już jest"
else
  curl -fsSL -o "$work_dir/kubectl" \
    "https://dl.k8s.io/release/$DF_KUBECTL_VERSION/bin/linux/amd64/kubectl"
  verify "$work_dir/kubectl" "$DF_KUBECTL_SHA256"
  install_binary kubectl "$work_dir/kubectl"
fi

if already_installed k3d "$DF_K3D_VERSION" version; then
  df_log "k3d $DF_K3D_VERSION już jest"
else
  curl -fsSL -o "$work_dir/k3d" \
    "https://github.com/k3d-io/k3d/releases/download/$DF_K3D_VERSION/k3d-linux-amd64"
  verify "$work_dir/k3d" "$DF_K3D_SHA256"
  install_binary k3d "$work_dir/k3d"
fi

if already_installed helm "v$DF_HELM_VERSION" version; then
  df_log "helm $DF_HELM_VERSION już jest"
else
  curl -fsSL -o "$work_dir/helm.tgz" \
    "https://get.helm.sh/helm-v$DF_HELM_VERSION-linux-amd64.tar.gz"
  verify "$work_dir/helm.tgz" "$DF_HELM_SHA256"
  tar -xzf "$work_dir/helm.tgz" -C "$work_dir" linux-amd64/helm
  install_binary helm "$work_dir/linux-amd64/helm"
fi

if already_installed kubeconform "${DF_KUBECONFORM_VERSION#v}" -v; then
  df_log "kubeconform $DF_KUBECONFORM_VERSION już jest"
else
  curl -fsSL -o "$work_dir/kubeconform.tgz" \
    "https://github.com/yannh/kubeconform/releases/download/$DF_KUBECONFORM_VERSION/kubeconform-linux-amd64.tar.gz"
  verify "$work_dir/kubeconform.tgz" "$DF_KUBECONFORM_SHA256"
  tar -xzf "$work_dir/kubeconform.tgz" -C "$work_dir" kubeconform
  install_binary kubeconform "$work_dir/kubeconform"
fi

case ":$PATH:" in
  *":$tools_dir:"*) ;;
  *) echo "UWAGA: $tools_dir nie jest w PATH — dodaj go w ~/.bashrc." >&2 ;;
esac
