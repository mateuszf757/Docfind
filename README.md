# DOCFIND

Wyszukiwarka dokumentów z odpowiedziami generowanymi przez model, prowadzona na Kubernetesie
w modelu GitOps i dostarczana do klienta jako chart Helma.

Projekt jest budowany warstwa po warstwie, a nie w jednym kawałku — każdy etap ma warunek
zakończenia dający się sprawdzić bez oceniającego. Uzasadnienia wyborów technicznych,
razem z ich kosztami, siedzą w [docs/DECYZJE.md](docs/DECYZJE.md).

## Stan

Etap 0 z 11 — szkielet repozytorium i pipeline. Zrobione:

- `/version` raportuje wersję z `git describe` i commit zgodny z HEAD
- obraz jest samoopisujący się: `version.json` powstaje przy budowaniu z danych z gita
- build wydania z brudnego drzewa jest odrzucany
- lint, format i testy jednostkowe uruchamiane tym samym skryptem lokalnie i w CI

## Struktura

```
services/api/      Usługa API — Dockerfile, kod, testy
ci/                lib.sh (wersjonowanie), run-tests.sh, build.sh
tests/corpus/      Deterministyczny korpus dla testów e2e
docs/              DECYZJE.md i dokumentacja operacyjna
dokumenty/         Roboczy korpus do indeksowania (poza repozytorium)
```

## Praca lokalna

Wymagania: Docker, [uv](https://docs.astral.sh/uv/), Python 3.12.

```bash
./ci/run-tests.sh          # lint, format, testy jednostkowe
./ci/build.sh api          # build obrazu z wersją z gita
RELEASE=1 ./ci/build.sh api  # build wydania — odrzuca brudne drzewo
```

Podgląd działającej usługi:

```bash
docker run --rm -p 8000:8000 ghcr.io/mateuszf757/docfind-api:$(git rev-parse --short=12 HEAD)
curl -s localhost:8000/version
```

## Etapy

| Etap | Zakres | Warunek zakończenia |
|---|---|---|
| 0 ✅ | Repozytorium i pipeline | PR uruchamia testy; build daje `version.json` zgodny z commitem |
| 1 | API w kontenerze, walidacja konfiguracji, `/metrics` | `docker stop` < 1 s; obraz < 200 MB; zły config = czytelna odmowa startu |
| 2 | k3d, Deployment, probe'y, 2 repliki, PDB | `kubectl drain` węzła → zero błędów w pętli curl |
| 3 | ingress-nginx, cert-manager (DNS-01), streaming | wymuszone odnowienie certyfikatu przechodzi bez ingerencji |
| 4 | Vault i External Secrets Operator | rotacja sekretu dociera do podów; zero jawnych sekretów w gicie |
| 5 | ArgoCD, app-of-apps, sync waves | ręczne `kubectl delete deploy` → ArgoCD odtwarza stan |
| 6 | ECK, Elasticsearch, ingest | reindeks z podmianą aliasu bez przestoju |
| 7 | LLM: `ollama \| vllm \| stub`, model z PVC | streaming przez HTTPS; przekroczenie kontekstu = czytelny błąd |
| 8 | kube-prometheus-stack, alerty, dashboardy jako kod | alert wykrywa awarię, której nie planowałem |
| 9 | Pełne CD i testy e2e | ten sam digest na obu klastrach |
| 10 | Chart OCI, drugi klaster jako „klient" | instalacja z rejestru; `helm rollback` < 15 min |
| 11 | Backup, runbook, sesje diagnostyczne | piąty sabotaż rozwiązany w < 15 min |
