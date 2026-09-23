# DOCFIND

Wyszukiwarka dokumentów z odpowiedziami generowanymi przez model, prowadzona na Kubernetesie
w modelu GitOps i dostarczana do klienta jako chart Helma.

Projekt jest budowany warstwa po warstwie, a nie w jednym kawałku — każdy etap ma warunek
zakończenia dający się sprawdzić bez oceniającego. Uzasadnienia wyborów technicznych,
razem z ich kosztami, siedzą w [docs/DECYZJE.md](docs/DECYZJE.md).

## Stan

Etap 2 z 11 — usługa na Kubernetesie. Drain każdego węzła z repliką API:
1358 żądań w trzech przebiegach, zero nieudanych.

- chart Helma z dwiema replikami, PDB, rozłożeniem na węzły i preStop
- własny CoreDNS z charta w dwóch replikach — wbudowany w k3s odcinał DNS
  całemu klastrowi przy drainie swojego węzła (decyzja 18)
- klaster lokalny wystawiony wyłącznie na loopback, bez rozluźniania
  zabezpieczeń hosta (decyzja 17)
- manifesty sprawdzane kubeconformem i politykami jako kodem
  (`ci/check_policy.py`) — poprawne względem schematu nie znaczy zgodne
  z decyzjami
- wszystko z zewnątrz przypięte niezmiennie: binarki i charty sumą, obrazy
  digestem, akcje GitHuba SHA commita; aktualizacje proponuje Dependabot
  (decyzja 20)
- build powtarzalny bajt w bajt, także między lokalnym BuildKitem a tym
  z CI — ten sam commit daje ten sam obraz i nie wywołuje rolloutu
  (decyzja 21, `ci/check-reproducible.sh`)

Zrobione w Etapach 0–1:

- `/version` raportuje wersję z `git describe` i commit zgodny z HEAD
- obraz jest samoopisujący się i waży 120 MB; buduje się z `uv.lock`, więc
  zawiera dokładnie wersje, które przeszły testy
- konfiguracja walidowana modelem Pydantic — nieznany klucz albo zła wartość
  kończy się odmową startu z komunikatem wskazującym pole
- `app.schema.json` generowany z modelu, a CI pilnuje, żeby był aktualny
- `get_secret()` jako jedyne wejście do sekretów, gotowe na Vaulta z Etapu 4
- `/metrics` z licznikiem żądań i histogramem czasu, z etykietą trasy jako
  szablonem — bez tego skan katalogów wysadziłby kardynalność
- build wydania z brudnego drzewa jest odrzucany
- warunki zakończenia etapu sprawdzane skryptem, nie na słowo

## Struktura

```
services/api/      Usługa API — Dockerfile, kod, testy
deploy/config/     app.yml.example i generowany app.schema.json
deploy/charts/     Chart Helma docfind
deploy/platform/   Wartości komponentów platformy (CoreDNS)
deploy/k3d/        Definicja lokalnego klastra (1 serwer, 2 węzły robocze)
ci/                Skrypty budowania, testów, wdrożenia i warunków zakończenia
tests/corpus/      Deterministyczny korpus dla testów e2e
docs/              DECYZJE.md i dokumentacja operacyjna
dokumenty/         Roboczy korpus do indeksowania (poza repozytorium)
```

## Praca lokalna

Wymagania i sposób ich sprawdzenia: [docs/WYMAGANIA.md](docs/WYMAGANIA.md).
Do testów wystarczą Docker i [uv](https://docs.astral.sh/uv/); do klastra
także narzędzia z `ci/install-tools.sh`.

```bash
./ci/run-tests.sh            # lint, format, testy jednostkowe
./ci/gen-schema.sh           # regeneracja app.schema.json z modelu
./ci/build.sh api            # build obrazu z wersją z gita
RELEASE=1 ./ci/build.sh api  # build wydania — odrzuca brudne drzewo
./ci/check-runtime.sh api    # warunki zakończenia Etapu 1
./ci/check-reproducible.sh api  # dwa buildy od zera → identyczny obraz

./ci/install-tools.sh        # kubectl, k3d, helm, kubeconform w przypiętych wersjach
./ci/deploy-local.sh         # klaster k3d + build + helm upgrade --install
./ci/check-drain.sh          # warunek zakończenia Etapu 2
./ci/deploy-local.sh --down  # usunięcie klastra
```

Podgląd działającej usługi. Konfiguracja jest wymagana — bez niej kontener
świadomie odmawia startu:

```bash
mkdir -p /tmp/docfind && cp deploy/config/app.yml.example /tmp/docfind/app.yml
docker run --rm -p 8000:8000 -v /tmp/docfind:/app/config:ro \
  ghcr.io/mateuszf757/docfind-api:$(git rev-parse --short=12 HEAD)
curl -s localhost:8000/version
curl -s localhost:8000/metrics
```

## Etapy

| Etap | Zakres | Warunek zakończenia |
|---|---|---|
| 0 ✅ | Repozytorium i pipeline | PR uruchamia testy; build daje `version.json` zgodny z commitem |
| 1 ✅ | API w kontenerze, walidacja konfiguracji, `/metrics` | obraz 120 MB < 200 MB; zły config = czytelna odmowa startu; zamykanie 0,37 s ponad narzut Dockera |
| 2 ✅ | k3d, Deployment, probe'y, 2 repliki, PDB | drain każdego węzła: 1358 żądań, 0 błędów |
| 3 | ingress-nginx, cert-manager (DNS-01), streaming | wymuszone odnowienie certyfikatu przechodzi bez ingerencji |
| 4 | Vault i External Secrets Operator | rotacja sekretu dociera do podów; zero jawnych sekretów w gicie |
| 5 | ArgoCD, app-of-apps, sync waves | ręczne `kubectl delete deploy` → ArgoCD odtwarza stan |
| 6 | ECK, Elasticsearch, ingest | reindeks z podmianą aliasu bez przestoju |
| 7 | LLM: `ollama \| vllm \| stub`, model z PVC | streaming przez HTTPS; przekroczenie kontekstu = czytelny błąd |
| 8 | kube-prometheus-stack, alerty, dashboardy jako kod | alert wykrywa awarię, której nie planowałem |
| 9 | Pełne CD i testy e2e | ten sam digest na obu klastrach |
| 10 | Chart OCI, drugi klaster jako „klient" | instalacja z rejestru; `helm rollback` < 15 min |
| 11 | Backup, runbook, sesje diagnostyczne | piąty sabotaż rozwiązany w < 15 min |
