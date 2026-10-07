# DOCFIND

Wyszukiwarka dokumentów z odpowiedziami generowanymi przez model, prowadzona na Kubernetesie
w modelu GitOps i dostarczana do klienta jako chart Helma.

Projekt jest budowany warstwa po warstwie, a nie w jednym kawałku — każdy etap ma warunek
zakończenia dający się sprawdzić bez oceniającego. Uzasadnienia wyborów technicznych,
razem z ich kosztami, siedzą w [docs/DECYZJE.md](docs/DECYZJE.md).

## Stan

Etap 3 z 11 (`v0.3.0`) — wejście przez Gateway API z TLS. Wymuszone odnowienie
certyfikatu pod ruchem: własne CA 59 żądań, Let's Encrypt staging 126 żądań —
zero nieudanych, proxy podaje nowy certyfikat bez restartu.

- Envoy Gateway zamiast ingress-nginx, który od marca 2026 nie dostaje łatek
  bezpieczeństwa (decyzja 11)
- cert-manager z własnym CA i z Let's Encrypt przez DNS-01 w Cloudflare —
  zaufany certyfikat dla klastra bez wejścia z internetu (decyzja 4)
- identyfikator żądania nadawany przez proxy, odsyłany przez API, w logu
  dostępowym JSON razem z czasem backendu (decyzja 29)
- limit bezczynności połączeń proxy→backend krótszy niż keep-alive API —
  chart odmawia odwrotnej relacji, która daje sporadyczne 502
- zasoby z CRD walidowane schematami z tych samych CRD, które instalujemy
  (decyzja 28); polityki sprawdzane też na żywych podach proxy, których nie
  widać w `helm template`
- Pod Security `restricted` na przestrzeni nazw aplikacji — API server
  odrzuca pod bez pełnego `securityContext`, także sondę drainu (decyzja 31)
- obraz publikowany z atestacją pochodzenia, sprawdzaną w rejestrze razem
  z tym, że to obraz przetestowany; digest w wyjściach zadania `build`
  (korekta decyzji 21); po wdrożeniu `/version` sprawdzany na każdym podzie
- wersje z tagów `vX.Y.Z`, numer minor to etap (decyzja 30)
- logika narzędzi i bramek przechodzi z Basha do Go (`tools/`, program `dft`):
  wersja z gita jest już liczona w Go i testowana na prawdziwych
  repozytoriach, przypięcia są plikiem danych `ci/pins.env`, a binarka
  narzędzi jest powtarzalna bajt w bajt (decyzja 23)

Etap 2 — usługa na Kubernetesie. Drain każdego węzła z repliką API:
1358 żądań w trzech przebiegach, zero nieudanych; dziś jeden bieg
`cluster:drain` drenuje kolejno każdy węzeł, który ma replikę (1385 żądań,
zero nieudanych), i sprząta po sobie także po przerwaniu.

- chart Helma z dwiema replikami, PDB, rozłożeniem na węzły i preStop
- własny CoreDNS z charta w dwóch replikach — wbudowany w k3s odcinał DNS
  całemu klastrowi przy drainie swojego węzła (decyzja 18)
- klaster lokalny wystawiony wyłącznie na loopback, bez rozluźniania
  zabezpieczeń hosta (decyzja 17)
- manifesty sprawdzane kubeconformem i politykami jako kodem
  (`tools/internal/policy`) — poprawne względem schematu nie znaczy zgodne
  z decyzjami
- wszystko z zewnątrz przypięte niezmiennie: narzędzia sumą w `mise.lock`,
  charty sumą, obrazy i BuildKit digestem, akcje GitHuba SHA commita, runner
  wersją systemu, schematy kubeconform commitem; aktualizacje proponuje
  Dependabot (decyzje 20 i 23)
- łatki od Dependabota — także odświeżenia digestu obrazu bazowego — scalane
  automatycznie po przejściu CI, z siedmiodniowym cooldownem; minor i major
  czekają na przegląd, a nowe wydanie Alpine pod tym samym tagiem zatrzymuje
  build (decyzja 22)
- testy jednostkowe biegną dwa razy: na hoście i w obrazie na musl, na tej
  samej bazie co produkcja (decyzja 24)
- build powtarzalny bajt w bajt, także między lokalnym BuildKitem a tym
  z CI — ten sam commit daje ten sam obraz i nie wywołuje rolloutu
  (decyzja 21, `dft check reproducible`)

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
deploy/charts/platform/  Chart platformy: Gateway, TLS, wydawcy certyfikatów
deploy/platform/   Wartości komponentów platformy (CoreDNS, cert-manager, Envoy Gateway)
deploy/k3d/        Definicja lokalnego klastra (1 serwer, 2 węzły robocze)
tools/             Narzędzia i bramki w Go: program dft (tools/cmd/dft), logika w tools/internal
ci/                ci/dft (buduje i uruchamia dft) i ci/pins.env (przypięcia)
deploy/environments/  Definicje środowisk dev, ci, staging, prod czytane przez dft
bin/mise           Launcher mise w przypiętej wersji — narzędzia i zadania (mise.toml)
tests/corpus/      Deterministyczny korpus dla testów e2e
docs/              DECYZJE.md i dokumentacja operacyjna
dokumenty/         Roboczy korpus do indeksowania (poza repozytorium)
```

## Ustawienia repozytorium

Reguły gałęzi `main` i tagów `v*`, włączenie auto-merge i wymóg przypinania
akcji pełnym SHA są zapisane jako kod (`.github/rulesets/`) i stosowane
skryptem — raz i po każdej zmianie reguł, przez osobę z uprawnieniami admina:

```bash
./bin/mise exec -- gh auth login
./bin/mise run repo:settings -- --dry-run   # podgląd zmian
./bin/mise run repo:settings
```

Bez reguły `main` workflow auto-merge łatek od Dependabota nie ma bramki:
`--auto` scaliłby PR natychmiast (decyzja 22).

Auto-merge scala tokenem aplikacji GitHuba — scalenie przez `GITHUB_TOKEN`
nie uruchomiłoby CI na `main`, więc obraz dla takiego commita nie zostałby
zbudowany ani opublikowany. Jednorazowo:

1. Utwórz aplikację GitHuba (Settings → Developer settings → GitHub Apps):
   bez webhooka, uprawnienia repozytorium *Contents: Read and write*
   i *Pull requests: Read and write*, instalacja tylko na tym koncie.
2. Zainstaluj ją wyłącznie na repozytorium `Docfind`.
3. Zapisz jej Client ID i klucz prywatny jako **sekrety Dependabota** —
   PR od Dependabota nie widzi zwykłych sekretów Actions:

```bash
./bin/mise exec -- gh secret set DOCFIND_AUTOMERGE_CLIENT_ID --app dependabot --body '<Client ID>'
./bin/mise exec -- gh secret set DOCFIND_AUTOMERGE_PRIVATE_KEY --app dependabot < klucz.pem
```

Bez tych sekretów workflow niczego nie scala i zostawia ostrzeżenie w PR-ze.

## Wydania

Wydanie to tag adnotowany `vX.Y.Z` na commicie z `main`. Numer minor to
ukończony etap (`v0.3.0` = Etap 3), łatka to poprawki w jego obrębie
(decyzja 30). Tagów `v*` pilnuje reguła: po wypchnięciu nie da się ich
przesunąć ani usunąć, więc pomyłkę poprawia się kolejnym numerem.

```bash
git switch main && git pull --ff-only
git tag --annotate v0.3.1 -m "Etap 3: <co zawiera wydanie>"
git push origin v0.3.1
```

Tag uruchamia w CI build wydania i publikuje
`ghcr.io/mateuszf757/docfind-api:0.3.1` z atestacją pochodzenia; digest jest
w podsumowaniu zadania `build`. Obrazy z `main` między tagami mają wersję
`0.3.2-dev.<commity od tagu>+<sha>` i tag `<sha12>`.

```bash
docker buildx imagetools inspect ghcr.io/mateuszf757/docfind-api:0.3.1 --format '{{json .Provenance}}'
```

## Praca lokalna

Wymagania i sposób ich sprawdzenia: [docs/WYMAGANIA.md](docs/WYMAGANIA.md).
Potrzebne są Docker, [uv](https://docs.astral.sh/uv/) i Python 3; resztę
narzędzi, w tym Go dla `tools/`, instaluje `./bin/mise` w wersjach i z sumami
z `mise.lock`, do katalogu `.mise/` w repozytorium — bez globalnej instalacji
i bez sudo.

```bash
./bin/mise install                # narzędzia z mise.lock
./bin/mise tasks                  # lista zadań
./bin/mise run test               # wersje, lint, typy, testy, polityki, kod Go — jak zadanie test w CI
./bin/mise run ci                 # cały pipeline po kolei, jak w CI (bez publikacji)
./bin/mise run build              # build obrazu z wersją z gita
RELEASE=1 ./bin/mise run build    # build wydania — tylko czyste drzewo na tagu vX.Y.Z
./bin/mise run test:image         # testy jednostkowe w obrazie na musl
./bin/mise run check:runtime      # warunki zakończenia Etapu 1 (raport JSON w .cache/reports)
./bin/mise run test:negatives     # warianty, które bramki Etapu 1 muszą odrzucić
./bin/mise run check:reproducible # build z cache i od zera → identyczny obraz
./bin/mise run check:tools        # to samo dla narzędzi Go → identyczna binarka
./bin/mise exec -- ci/dft --help  # polecenia dft (wersja, tożsamość, bramki)

./bin/mise run cluster:up         # klaster k3d + build + helm upgrade --install
./bin/mise run cluster:up -- --image ghcr.io/mateuszf757/docfind-api@sha256:…   # obraz z rejestru po digeście
./bin/mise run cluster:drain      # warunek zakończenia Etapu 2
./bin/mise run cluster:tls        # warunek zakończenia Etapu 3: odnowienie certyfikatu pod ruchem
./bin/mise run cluster:identity   # na każdym podzie obraz, wersja i commit z tego drzewa
./bin/mise run test:cluster       # to, co API server ma odrzucić (sonda bez securityContext)
./bin/mise run dns-token          # token Cloudflare dla Let's Encrypt (DNS-01), raz
./bin/mise run cluster:down       # usunięcie klastra
./bin/mise exec -- kubectl get pods -A   # kubectl z przypiętej wersji, kubeconfig projektu
```

Zadania wołają `ci/dft` — program w Go z `tools/`, budowany z bieżącego drzewa
przy każdym wywołaniu (bez zmian w kodzie nic się nie kompiluje). Bez mise
działa, jeśli w PATH są Go 1.27.1 i pozostałe narzędzia. Klaster zapisuje dane
dostępowe do `.cache/kubeconfig`, nie do `~/.kube/config`; `dft` bierze ścieżkę
i kontekst z definicji środowiska, a `./bin/mise exec` ustawia `KUBECONFIG`
sam.

### Środowiska

Zadania klastrowe działają na środowisku `dev`, chyba że `MISE_ENV` wskaże
inne — mise nakłada wtedy `mise.<nazwa>.toml` z `DOCFIND_ENV` i `KUBECONFIG`
tego środowiska:

```bash
MISE_ENV=ci ./bin/mise run cluster:up -- --image-archive obraz.tar --config-digest sha256:…
MISE_ENV=staging ./bin/mise run cluster:drain
MISE_ENV=staging ./bin/mise exec -- kubectl get pods -A
```

| | `dev` | `ci` | `staging` | `prod` |
|---|---|---|---|---|
| Klaster | k3d `docfind` | k3d `docfind-ci`, efemeryczny w CI | k3d `docfind-staging`, długo żyjący | do Etapu 10 brak |
| Obraz | z drzewa, archiwum albo rejestr | artefakt zadania build | digest z rejestru | digest wydania |
| TLS | z `mise.local.toml` | własne CA | Let's Encrypt staging | Let's Encrypt |

Definicja jest listą dozwolonych operacji: `dft` odmawia wszystkiego, czego
środowisko nie wymienia — na `prod` niczego, na `staging` usunięcia klastra
(decyzja 35).

Podgląd działającej usługi. Konfiguracja jest wymagana — bez niej kontener
świadomie odmawia startu:

```bash
mkdir -p /tmp/docfind && cp deploy/config/app.yml.example /tmp/docfind/app.yml
docker run --rm -p 127.0.0.1:8000:8000 -v /tmp/docfind:/app/config:ro \
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
| 3 ✅ | Envoy Gateway (Gateway API), cert-manager: własne CA i Let's Encrypt DNS-01 | odnowienie pod ruchem: 59 i 126 żądań, 0 błędów |
| 4 | Vault i External Secrets Operator | rotacja sekretu dociera do podów; zero jawnych sekretów w gicie |
| 5 | ArgoCD, app-of-apps, sync waves | ręczne `kubectl delete deploy` → ArgoCD odtwarza stan |
| 6 | ECK, Elasticsearch, ingest | reindeks z podmianą aliasu bez przestoju |
| 7 | LLM: `ollama \| vllm \| stub`, model z PVC | streaming przez HTTPS; przekroczenie kontekstu = czytelny błąd |
| 8 | kube-prometheus-stack, alerty, dashboardy jako kod | alert wykrywa awarię, której nie planowałem |
| 9 | Pełne CD i testy e2e | ten sam digest na obu klastrach |
| 10 | Chart OCI, drugi klaster jako „klient" | instalacja z rejestru; `helm rollback` < 15 min |
| 11 | Backup, runbook, sesje diagnostyczne | piąty sabotaż rozwiązany w < 15 min |
