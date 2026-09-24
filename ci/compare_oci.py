"""Porównanie dwóch obrazów zapisanych przez `docker save` jako układ OCI.

    python3 ci/compare_oci.py <katalog-a> <katalog-b>

Kody wyjścia: 0 — obrazy identyczne, 1 — obrazy się różnią, 2 — porównanie
się nie wykonało (zły układ katalogu, brak pliku). Rozdzielone, bo awaria
narzędzia nie może wyglądać jak wynik. Tylko biblioteka standardowa, żeby
działał wszędzie tam, gdzie jest python3.

Porównywane są manifesty platform, czyli to, co faktycznie trafia do
kontenera: konfiguracja i warstwy. Manifesty atestacji (provenance) są
raportowane osobno i nie wpływają na werdykt — z definicji opisują
przebieg budowania, a nie zawartość obrazu.

Przy różnicy skrypt schodzi do poziomu pliku: które warstwy się różnią
i które pliki w nich. Tak znaleziony został uv_cache.json z czasem
instalacji w nanosekundach — jedyny bajt różniący dwa buildy tego samego
commita.
"""

from __future__ import annotations

import gzip
import hashlib
import io
import json
import sys
import tarfile
from pathlib import Path
from typing import Any

ATTESTATION = "attestation-manifest"


def blob(root: Path, digest: str) -> bytes:
    return (root / "blobs" / "sha256" / digest.removeprefix("sha256:")).read_bytes()


def manifests(root: Path) -> dict[str, dict[str, Any]]:
    """Manifesty z indeksu, kluczowane platformą albo rodzajem atestacji."""
    top = json.loads((root / "index.json").read_text())["manifests"][0]
    node = json.loads(blob(root, top["digest"]))
    entries = node.get("manifests", [top])

    found = {}
    for entry in entries:
        annotations = entry.get("annotations", {})
        manifest = json.loads(blob(root, entry["digest"]))
        if annotations.get("vnd.docker.reference.type") == "attestation-manifest":
            key = ATTESTATION
        else:
            # Bez atestacji docker save zapisuje sam manifest zamiast indeksu,
            # a wtedy platformy nie ma we wpisie — jest w konfiguracji obrazu.
            platform = entry.get("platform") or json.loads(blob(root, manifest["config"]["digest"]))
            key = f"{platform.get('os', '?')}/{platform.get('architecture', '?')}"
        found[key] = {"digest": entry["digest"], "manifest": manifest}
    return found


def layer_files(root: Path, digest: str) -> dict[str, str]:
    """Ścieżka pliku w warstwie → suma jego zawartości (albo cel dowiązania)."""
    raw = blob(root, digest)
    if raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    files = {}
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        for member in archive:
            if member.isfile():
                extracted = archive.extractfile(member)
                content = extracted.read() if extracted else b""
                files[member.name] = hashlib.sha256(content).hexdigest()
            elif member.issym() or member.islnk():
                files[member.name] = f"-> {member.linkname}"
            else:
                files[member.name] = f"{member.type!r} {member.mode:o} {member.uid}:{member.gid}"
    return files


def explain_layer(a: Path, b: Path, digest_a: str, digest_b: str) -> list[str]:
    files_a, files_b = layer_files(a, digest_a), layer_files(b, digest_b)
    lines = []
    for path in sorted(files_a.keys() | files_b.keys()):
        if files_a.get(path) != files_b.get(path):
            lines.append(f"      {path}")
    if not lines:
        lines.append("      (pliki te same — różnią się metadane archiwum: czasy, prawa)")
    return lines


EXIT_IDENTICAL, EXIT_DIFFERENT, EXIT_ERROR = 0, 1, 2


def main() -> int:
    if len(sys.argv) != 3:
        print("użycie: compare_oci.py <katalog-a> <katalog-b>", file=sys.stderr)
        return EXIT_ERROR
    try:
        return compare(Path(sys.argv[1]), Path(sys.argv[2]))
    except (OSError, KeyError, IndexError, ValueError, tarfile.TarError) as exc:
        print(f"BŁĄD porównania: {type(exc).__name__}: {exc}", file=sys.stderr)
        return EXIT_ERROR


def compare(a: Path, b: Path) -> int:
    ours, theirs = manifests(a), manifests(b)
    identical = True

    for key in sorted(ours.keys() | theirs.keys()):
        left, right = ours.get(key), theirs.get(key)
        if left is None or right is None:
            # Atestacja obecna tylko po jednej stronie to różnica w sposobie
            # budowania (lokalnie jej nie ma, w rejestrze jest), nie w zawartości.
            if key == ATTESTATION:
                print(f"         {key}: obecna tylko w jednym obrazie — nie wpływa na werdykt")
                continue
            print(f"RÓŻNICA  {key}: obecny tylko w jednym obrazie")
            identical = False
            continue

        same = left["digest"] == right["digest"]
        if key == ATTESTATION:
            note = "identyczna" if same else "różna — oczekiwane, opisuje przebieg budowania"
            print(f"         {key}: {note}")
            continue

        if same:
            print(f"OK       {key}: {left['digest'][:19]}")
            continue

        identical = False
        print(f"RÓŻNICA  {key}: {left['digest'][:19]} ≠ {right['digest'][:19]}")
        layers_a = left["manifest"]["layers"]
        layers_b = right["manifest"]["layers"]
        for index, (la, lb) in enumerate(zip(layers_a, layers_b, strict=False)):
            if la["digest"] != lb["digest"]:
                print(f"   warstwa {index}: różne pliki")
                print("\n".join(explain_layer(a, b, la["digest"], lb["digest"])))
        if len(layers_a) != len(layers_b):
            print(f"   różna liczba warstw: {len(layers_a)} ≠ {len(layers_b)}")
        if left["manifest"]["config"]["digest"] != right["manifest"]["config"]["digest"]:
            print("   konfiguracja obrazu różna (także wtedy, gdy różni się tylko lista warstw)")

    return EXIT_IDENTICAL if identical else EXIT_DIFFERENT


if __name__ == "__main__":
    raise SystemExit(main())
