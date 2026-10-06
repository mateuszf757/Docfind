"""Schematy JSON dla kubeconform, generowane z definicji CRD.

    helm template ... --include-crds | python3 ci/crd_schemas.py <katalog>

Zapisuje <katalog>/<grupa>/<rodzaj>_<wersja>.json dla każdej wersji każdego
CRD na wejściu — w układzie, którego oczekuje kubeconform:

    -schema-location '<katalog>/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json'

Schematy powstają z tych samych CRD, które instalują przypięte charty, a nie
z zewnętrznego katalogu. Gotowe katalogi schematów CRD nie nadążają za
wydaniami — walidacja względem starszej wersji CRD przepuszczałaby pola, których
API server nie przyjmie, albo odrzucała nowe.

Tryb ścisły: obiekty z listą właściwości dostają additionalProperties: false,
chyba że CRD jawnie dopuszcza nieznane pola (x-kubernetes-preserve-unknown-fields).
Literówka w nazwie pola zasobu jest wtedy błędem, a nie polem po cichu
pominiętym przez API server — tak jak -strict dla zasobów wbudowanych.

Tylko biblioteka standardowa i PyYAML.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import yaml


def strict(schema: Any) -> Any:
    """Kopia schematu OpenAPI v3 w postaci, którą rozumie walidator JSON Schema."""
    if isinstance(schema, list):
        return [strict(item) for item in schema]
    if not isinstance(schema, dict):
        return schema

    converted = {key: strict(value) for key, value in schema.items()}

    # OpenAPI `nullable` nie istnieje w JSON Schema — null trzeba dopuścić typem.
    if converted.pop("nullable", False) and isinstance(converted.get("type"), str):
        converted["type"] = [converted["type"], "null"]

    preserves_unknown = converted.get("x-kubernetes-preserve-unknown-fields") is True
    closes_object = "properties" in converted and "additionalProperties" not in converted
    if closes_object and not preserves_unknown:
        converted["additionalProperties"] = False

    return converted


def write_schemas(documents: list[Any], target: Path) -> int:
    written = 0
    for document in documents:
        if not isinstance(document, dict) or document.get("kind") != "CustomResourceDefinition":
            continue
        spec = document["spec"]
        group = spec["group"]
        kind = spec["names"]["kind"].lower()
        for version in spec["versions"]:
            schema = version.get("schema", {}).get("openAPIV3Schema")
            if schema is None:
                continue
            path = target / group / f"{kind}_{version['name']}.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(strict(schema), indent=1, sort_keys=True), encoding="utf-8")
            written += 1
    return written


def main() -> int:
    if len(sys.argv) != 2:
        print("użycie: crd_schemas.py <katalog-docelowy> < crd.yaml", file=sys.stderr)
        return 2
    target = Path(sys.argv[1])
    documents = list(yaml.safe_load_all(sys.stdin))
    written = write_schemas(documents, target)
    if written == 0:
        print("BŁĄD: na wejściu nie było żadnego CRD ze schematem", file=sys.stderr)
        return 1
    print(f"{written} schematów CRD w {target}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
