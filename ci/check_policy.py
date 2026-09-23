"""Polityki dla wyrenderowanych manifestów Kubernetesa.

    helm template ... | python ci/check_policy.py <nazwa źródła> [--require-digest]

kubeconform sprawdza, czy manifest jest poprawny względem schematu. Tu
sprawdzamy, czy jest zgodny z naszymi decyzjami — pole może być poprawne
i jednocześnie niechciane. Przykład, który dał początek temu plikowi: chart
CoreDNS dokładał domyślny limit CPU, bo Helm scala wartości z domyślnymi,
a kubeconform nie miał czego zgłosić.

Każda reguła odwołuje się do decyzji w docs/DECYZJE.md.
"""

from __future__ import annotations

import sys
from collections.abc import Iterator
from typing import Any

import yaml

WORKLOAD_KINDS = {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}


def pod_spec(manifest: dict[str, Any]) -> dict[str, Any] | None:
    if manifest.get("kind") not in WORKLOAD_KINDS:
        return None
    spec = manifest.get("spec", {})
    if manifest["kind"] == "CronJob":
        spec = spec.get("jobTemplate", {}).get("spec", {})
    return spec.get("template", {}).get("spec")


def violations(manifest: dict[str, Any], *, require_digest: bool) -> Iterator[str]:
    spec = pod_spec(manifest)
    if spec is None:
        return

    name = f"{manifest['kind']}/{manifest['metadata']['name']}"
    for container in spec.get("containers", []) + spec.get("initContainers", []):
        where = f"{name} kontener {container['name']}"
        resources = container.get("resources", {})
        requests = resources.get("requests", {})
        limits = resources.get("limits", {})

        # Decyzja 16: request CPU i pamięci zawsze — na nich opiera się
        # scheduler; limit pamięci zawsze — chroni węzeł przed wyciekiem.
        for resource in ("cpu", "memory"):
            if resource not in requests:
                yield f"{where}: brak requests.{resource}"
        if "memory" not in limits:
            yield f"{where}: brak limits.memory"

        # Decyzja 16: bez limitu CPU — dławienie przez CFS daje opóźnienia
        # przy bezczynnym węźle.
        if "cpu" in limits:
            yield f"{where}: limits.cpu={limits['cpu']} — decyzja 16 wyklucza limity CPU"

        # Decyzja 20: obrazy z zewnątrz przypięte digestem. Tag jest przesuwalny,
        # więc sam tag oznacza zgodę na zawartość, której nikt nie sprawdził.
        if require_digest and "@sha256:" not in container.get("image", ""):
            yield f"{where}: obraz {container.get('image')} bez digestu — decyzja 20"


def main() -> int:
    arguments = sys.argv[1:]
    require_digest = "--require-digest" in arguments
    names = [argument for argument in arguments if not argument.startswith("--")]
    source = names[0] if names else "stdin"

    manifests = [doc for doc in yaml.safe_load_all(sys.stdin) if doc]
    found = [
        problem
        for manifest in manifests
        for problem in violations(manifest, require_digest=require_digest)
    ]

    for problem in found:
        print(f"POLITYKA ({source}): {problem}", file=sys.stderr)
    return 1 if found else 0


if __name__ == "__main__":
    raise SystemExit(main())
