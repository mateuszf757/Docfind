# Runbook DOCFIND

Procedury dla operatora: objaw, jak potwierdzić, co zrobić. Polecenia
działają na środowisku z `MISE_ENV` (domyślnie dev); kubeconfig projektu
podaje `./bin/mise exec --`.

## OpenBao — magazyn sekretów

Trzy repliki z Raftem w przestrzeni nazw `openbao`, odpieczętowanie
kluczem z Secreta `openbao/openbao-seal`, konfiguracja z
`deploy/platform/openbao` stosowana przy każdym `cluster up` (decyzja 42).

### Stan

```bash
./bin/mise run check:openbao      # repliki, Raft, restart standby; raport .cache/reports/openbao.json
./bin/mise exec -- kubectl -n openbao get pods -L openbao-active,controller-revision-hash
```

Zdrowy stan: trzy pody `Ready`, dokładnie jeden z `openbao-active=true`,
wszystkie na tej samej rewizji, Raft 3/3 głosujących.

### Pod nie wraca do gotowości po restarcie

**Objaw:** pod `Running`, `0/1`, sonda gotowości zwraca 503 (zapieczętowany)
albo 501 (niezainicjalizowany).

```bash
./bin/mise exec -- kubectl -n openbao logs openbao-N | grep -iE 'seal|raft|join'
./bin/mise exec -- kubectl -n openbao get secret openbao-seal
```

- `seal` w logu i brak Secreta — klucz odpieczętowania zniknął; dalej
  „Utrata klucza odpieczętowania".
- `failed to join raft cluster` — pod nie widzi lidera: sprawdź pozostałe
  pody i NetworkPolicy `openbao` (ruch 8200/8201 między replikami).
- Pod z pustym wolumenem (nowy węzeł, usunięty PVC) dołącza sam przez
  `retry_join` — wystarczy, że dwa pozostałe mają kworum.

### Utrata kworum Raft

**Objaw:** dwa z trzech podów niegotowe, żaden nie ma `openbao-active=true`,
ESO zgłasza błędy synchronizacji. Aplikacja działa dalej — Secrety
utworzone przez ESO zostają w klastrze, nie odświeżają się tylko nowe
wartości.

Najpierw przywróć pody (węzły, wolumeny) — Raft wybierze lidera sam, gdy
dwie repliki wrócą. Nie usuwaj wolumenów, żeby „zacząć od nowa": to dane.

### Utrata klucza odpieczętowania

**Objaw:** `dft cluster up` odmawia: „brak klucza odpieczętowania … przy
danych OpenBao". Działające pody trzymają klucz w pamięci, ale każdy
restart kończy się zapieczętowanym podem.

Bez klucza danych w Raft nie da się odczytać. Do Etapu 11 (kopia klucza
razem ze snapshotem) jedyna droga to odtworzenie magazynu:

1. Zapisz, co jest w OpenBao, z jego źródeł: token Cloudflare z menedżera
   haseł, sekrety aplikacji wygenerujesz od nowa.
2. Usuń wydanie i dane: `helm -n openbao uninstall openbao`,
   `kubectl -n openbao delete pvc -l app.kubernetes.io/name=openbao`.
3. `./bin/mise run cluster:up` — nowy klucz, inicjalizacja, konfiguracja.
4. Wpisz sekrety ponownie (`./bin/mise run dns-token`).

### Dostęp awaryjny — rola `platform-admin` nie działa

**Objaw:** `cluster up` kończy się „logowanie rolą platform-admin nie
przeszło". Token root nie istnieje (odwołany po pierwszej konfiguracji);
nowy powstaje z klucza odzyskiwania.

Klucz odzyskiwania: `~/.local/state/docfind/<klaster>/openbao-recovery.json`
(albo menedżer haseł, jeśli go tam przeniesiono). Token root pojawi się
w terminalu — rób to sam, nie przez narzędzie, które zapisuje wyjście.

```bash
./bin/mise exec -- kubectl -n openbao exec -it openbao-0 -- sh
# w podzie:
export BAO_CACERT=/openbao/tls/openbao-tls/ca.crt BAO_TLS_SERVER_NAME=openbao.openbao.svc
bao operator generate-root -init                 # Nonce i OTP
bao operator generate-root -nonce=<nonce>        # pyta o klucz odzyskiwania, bez echa
bao operator generate-root -decode=<encoded> -otp=<otp>   # token root
export BAO_TOKEN=<token root>
bao read auth/kubernetes/role/platform-admin     # co jest nie tak z rolą
# napraw (zwykle wystarczy wyjść i uruchomić `cluster up` z działającą rolą)
bao token revoke -self                           # token root zawsze odwołany na koniec
```

### Zmiana klucza odpieczętowania

Pieczęć `static` przyjmuje klucz bieżący i poprzedni. Nowy klucz to nowy
Secret (`openbao-seal-2`), w konfiguracji `current_key_id`/`current_key` na
nowy, a dotychczasowy jako `previous_key_id`/`previous_key`; `cluster up`
wymieni pody (nowa rewizja). Stary Secret usuń dopiero po wymianie
wszystkich podów.

## External Secrets Operator — sekrety z OpenBao w klastrze

ESO w przestrzeni nazw `external-secrets`; w każdej przestrzeni, która
czyta sekrety, własny SecretStore `openbao` z rolą tylko do swoich ścieżek
(decyzja 43).

### Stan

```bash
./bin/mise exec -- kubectl get secretstore,externalsecret -A
./bin/mise exec -- kubectl -n cert-manager describe externalsecret cloudflare-api-token
```

Zdrowy stan: SecretStore `Ready=True` (`Valid`), ExternalSecret `Ready=True`
(`SecretSynced`), Secret z `ownerReference` na swój ExternalSecret.

### ExternalSecret się nie synchronizuje

Najpierw powód z warunku `Ready` ExternalSecret i SecretStore:

- bez żadnego stanu — kontroler nie dociera do obiektu:
  `kubectl -n external-secrets logs deploy/external-secrets`; `connection
  refused` na adresie API servera to NetworkPolicy ESO (reguły egress
  w `deploy/platform/external-secrets/values.yaml`);
- `permission denied` z OpenBao — rola nie ma ścieżki albo konto nie to:
  polityka w `deploy/platform/openbao/policies/`, rola w `config.yaml`;
  audyt OpenBao (`kubectl -n openbao logs <aktywny pod>`) pokaże, kto i co
  próbował;
- `secret not found` — w OpenBao nie ma sekretu (token DNS: `dns-token`).

Wymuszenie synchronizacji po poprawce:
`kubectl -n <ns> annotate externalsecret <nazwa> force-sync=$(date +%s) --overwrite`.

### OpenBao niedostępny albo zapieczętowany

Secrety od ESO zostają w klastrze, więc wszystko, co je czyta, działa dalej;
nie dochodzą tylko nowe wartości. ESO zgłasza błąd synchronizacji i ponawia
— z narastającą przerwą, do kilkunastu minut. Naprawa — sekcja OpenBao
wyżej; po niej wymuś walidację magazynów, zamiast czekać na ESO:

```bash
./bin/mise exec -- kubectl annotate secretstore -A --all docfind.io/revalidate=$(date +%s) --overwrite
./bin/mise exec -- kubectl get secretstore,externalsecret -A
```

### Zmiana tokenu Cloudflare

Nowy token w Cloudflare, potem `./bin/mise run dns-token` (każde środowisko
z Let's Encrypt) — zapisuje nową wersję w OpenBao i czeka na Secret.
Poprzednia wersja zostaje w KV v2 do wglądu; stary token unieważnij
w Cloudflare dopiero po udanym `cluster:tls`.

## Sekrety aplikacji — token dostępu do /metrics

Tokeny w OpenBao (`docfind/app/metrics`, pole `tokens`, po jednym na
linię), w podzie jako plik `/run/secrets/metrics-token` czytany przy każdym
żądaniu (decyzja 44).

### Rotacja

```bash
./bin/mise run cluster:rotation
```

To pełna rotacja, nie symulacja: nowy token obok dotychczasowych, potem
tylko nowy, z pomiarem na każdym podzie i sprawdzeniem, że żaden się nie
zrestartował. Raport bez wartości tokenów:
`.cache/reports/rotation-<środowisko>.json`. Zmiana dociera po ok. minucie
na fazę.

### Pilnie — token wyciekł

Rotacja jak wyżej, a po zapisie w OpenBao wymuszenie synchronizacji zamiast
czekania na refreshInterval:
`kubectl -n docfind annotate externalsecret docfind-api-secrets force-sync=$(date +%s) --overwrite`.
Kubelet podmienia pliki w wolumenie w ciągu okresu synchronizacji poda
(do minuty); restart podów nie jest potrzebny.

### /metrics zwraca 503

Pod nie może odczytać `/run/secrets/metrics-token` — Secret zniknął albo
jest pusty. `kubectl -n docfind get externalsecret docfind-api-secrets`
i sekcja External Secrets Operator wyżej. Metryki są wtedy zamknięte,
a nie otwarte.

## Argo CD — dostawa aplikacji na staging

Argo CD w trybie Core w przestrzeni `argocd` (kontroler, repo-server,
Redis — bez serwera API i UI w klastrze). Application `docfind` śledzi
`ghcr.io/mateuszf757/charts/docfind` w zakresie z
`deploy/environments/staging.yaml` (`delivery.argocd.versions`),
z `selfHeal` i `prune` (decyzja 46). Chart publikuje CI na `main` po
bramkach klastra (decyzja 45). Polecenia poniżej z `MISE_ENV=staging`.

### Stan

```bash
MISE_ENV=staging ./bin/mise exec -- kubectl -n argocd get application docfind \
  -o jsonpath='{.status.sync.status} {.status.sync.revision} {.status.health.status}{"\n"}'
MISE_ENV=staging ./bin/mise run cluster:identity
MISE_ENV=staging ./bin/mise exec -- argocd admin dashboard -n argocd --port 8090   # UI na localhost:8090 (8080 to HTTP klastra dev)
```

Zdrowy stan: `Synced <wersja> Healthy`, gdzie wersja to najwyższa w GHCR
spełniająca zakres (wersja z podsumowania ostatniego biegu `ci` na `main`,
zadanie `chart`), a `cluster:identity` przechodzi.

### Nowa wersja nie dociera

**Objaw:** CI opublikował chart, a Application stoi na starszej wersji.

```bash
MISE_ENV=staging ./bin/mise exec -- kubectl -n argocd get application docfind -o jsonpath='{.status.conditions}'
```

- `ComparisonError` z `401`/`403` przy tagach — pakiet `charts/docfind`
  w GHCR nie jest publiczny albo GHCR nie odpowiada;
- `version matching constraint not found` — zakres nie obejmuje żadnej
  opublikowanej wersji (literówka w `versions`, zły chart);
- `invalid semver version in tags` — w repozytorium charta jest tag, który
  parsuje się jako SemVer z błędem (np. zero wiodące w części
  przedwydaniowej); Argo przerywa wtedy rozwiązywanie — usuń ten tag w GHCR;
- bez warunku — Argo trzyma listę tagów w pamięci do 3 minut; odśwież:
  `kubectl -n argocd annotate application docfind argocd.argoproj.io/refresh=hard --overwrite`.

`MISE_ENV=staging ./bin/mise run cluster:up` liczy wersję z rejestru tak jak
Argo, wpuszcza jej obraz polityką stagingu, zleca odświeżenie i czeka do
10 minut, wypisując, na co czeka.

### Cofnięcie wdrożenia

Nie `helm rollback` ani `kubectl edit`/`set image` — `selfHeal` przywróci
stan z charta w kilka sekund. Przypnij ostatnią dobrą wersję w definicji:

```yaml
delivery:
  argocd:
    versions: "0.4.1-dev.12"
```

i `MISE_ENV=staging ./bin/mise run cluster:up` — do kroku 3 (app-of-apps)
Application zakłada `dft` z drzewa roboczego, więc zmiana działa od razu,
a PR z nią idzie zaraz potem. Powrót do śledzenia: `">=0.0.0-0"`.

Czasu cofnięcia nikt jeszcze nie zmierzył. Przy pierwszym użyciu zapisz
czas od zmiany definicji do `Synced <przypięta wersja> Healthy` i zielonego
`cluster:identity` — dla porównania warunek Etapu 10 to `helm rollback`
u klienta w < 15 min. Samo przejście `helm rollback` pod ruchem mierzy nocą
`cluster:upgrade` w `ci`: `rollout_seconds` w `upgrade-ci.json`.

### Ręczna zmiana „nie trzyma się"

Tak ma być: `selfHeal`. Zmiana idzie przez chart (PR) albo wartości
środowiska w definicji. Bramka, która to sprawdza (replika więcej, usunięty
PDB, bez przestoju): `MISE_ENV=staging ./bin/mise run cluster:selfheal`,
raport `.cache/reports/selfheal-staging.json`.

### `helm list -n docfind` jest pusty

Stan oczekiwany. Pierwszy `cluster:up` z Argo CD na klastrze, gdzie
aplikację instalował Helm, usuwa zapis wydania Helma
(`sh.helm.release.v1.docfind.*`) po tym, jak Argo przejął zasoby — zasoby
zostają. `helm upgrade --install docfind` na stagingu postawiłby wydanie
obok Argo; `dft cluster up --image` na środowisku z Argo odmawia.

### Charty platformy w rejestrze

Zadanie `chart` na `main` publikuje chart platformy
(`charts/docfind-platform`) i lustro przypiętych chartów z zewnątrz
(`charts/platform/<przypięcie>`, decyzja 80). Raporty:
`.cache/reports/chart-platform.json` i `chart-mirror.json` w artefakcie
biegu, tabela w podsumowaniu.

- **Ostrzeżenie „pakiet nie jest publiczny".** Nowy pakiet w GHCR jest
  prywatny; Argo CD pobiera bez poświadczeń. Raz na pakiet: GitHub →
  Packages → pakiet → Package settings → Change visibility → Public.
- **„tag lustra nie zmienia treści".** Pod tą wersją w lustrze jest inny
  manifest niż złożony z archiwum z sumą z `ci/pins.env`. Nie usuwaj tagu,
  żeby przeszło — sprawdź, skąd różnica (podmienione archiwum upstreamu
  pod tą samą wersją albo zmiana adnotacji lustra w kodzie). Nowa treść to
  nowe przypięcie, z nową wersją.
- **Nowe przypięcie charta** w `ci/pins.env` trafia do lustra po scaleniu;
  poprzednia wersja zostaje. Lokalnie, na rejestrze tymczasowym:
  `DF_IMAGE_REGISTRY=127.0.0.1:5000/<właściciel> ./bin/mise run chart:mirror`.


## Wydanie — ćwiczenie promocji w repozytorium próbnym

Promocja na prod biegnie tylko na tagu `v*`, a tagów `v*` nie da się
usunąć ani przesunąć — każdy bieg `promote` w tym repozytorium to
prawdziwe wydanie. Ścieżkę przed pierwszym prawdziwym wydaniem ćwiczy się
w repozytorium próbnym. Ćwiczenie sprawdza:
- odczyt reguł środowiska tokenem zadania;
- czekanie na zgodę;
- Deployment;
- obie atestacje wydania — wydania i pakietu dowodów (decyzja 53).

Powtarzać przy zmianie zadania `promote` albo `stages/promote`.

**Tylko u innego właściciela** (organizacja albo drugie konto), nigdy jako
fork na tym samym koncie. Obraz i chart nazywają się
`ghcr.io/<właściciel>/docfind-api` i `ghcr.io/<właściciel>/charts/docfind`.
Na tym samym koncie próba pisałaby do prawdziwych pakietów, a Argo CD na
stagingu wdrożyłoby chart z próby (zakres `>=0.0.0-0`). Repozytorium
próbne nie dostaje sekretów — ścieżka wydania ich nie potrzebuje.

1. Utwórz repozytorium publiczne `<organizacja>/Docfind` (atestacje
   publicznego Sigstore, publiczny GHCR). Wypchnij `main`:
   `git push https://github.com/<organizacja>/Docfind.git main`.
2. Ustawienia z kodu w próbie (rulesety, środowiska z recenzentem):
   `GH_REPO=<organizacja>/Docfind ./bin/mise run repo:settings`.
   Recenzent z `.github/environments/production.json` musi mieć dostęp do
   repozytorium.
3. Poczekaj na zielony bieg `main` w próbie (obraz i chart w GHCR
   organizacji, atestacje). Digest jest w podsumowaniu `build-publish`.
4. Tag wydania bez stagingu — próba nie ma klastra, więc zapis akceptacji
   pisze się ręcznie w formacie `dft release`. CI sprawdza pochodzenie
   obrazu z `main`, pakiet dowodów i zgodę, a nie samą akceptację. Do
   pliku `docs/v0.0.1.txt`:

   ```text
   Ćwiczenie promocji

   Akceptacja na stagingu: ćwiczenie — bez stagingu

   Image: ghcr.io/<organizacja>/docfind-api@sha256:<digest z kroku 3>
   Commit: <pełny SHA commita z kroku 3>
   Accepted-At: <teraz, RFC 3339 UTC>
   Gates-Commit: <ten sam pełny SHA>
   ```

   `git tag --annotate --cleanup=verbatim --file=docs/v0.0.1.txt v0.0.1 <SHA>`
   i `git push https://github.com/<organizacja>/Docfind.git v0.0.1`.
   Tag zostaje tylko w próbie — w tym repozytorium nie pushuj go.
5. Oczekiwane w biegu tagu:
   - `release` — tag `0.0.1` w GHCR organizacji i pakiet dowodów w podsumowaniu;
   - `attest-release` — dwie atestacje;
   - `vulns` — zielone;
   - `promote` — czeka na zgodę w Environment `production`;
   - po zgodzie — Deployment `production` z linkiem do atestacji, a w zapisie promocji `evidence_run`.
6. Wariant negatywny: usuń recenzenta ze środowiska `production` w próbie
   i wydaj `v0.0.2` (krok 4) → `dft promote` kod 1 („środowisko bez
   wymaganej zgody”).
7. Wynik — numery biegów i to, co się nie zgadzało — dopisz w opisie
   najbliższego PR-a wydania. Repozytorium i pakiety próby usuń po
   ćwiczeniu.

## CI — czerwony `main` i noc

Zadanie `main-health` (każdy push na `main` i noc) prowadzi jedno issue
„Zdrowie main: czerwony bieg, brak nocy, bliskie daty”. Otwiera je, gdy
`main` jest czerwony dłużej niż doba, ostatni bieg nocny jest starszy niż
48 h albo wyjątek wygasa w ciągu tygodnia. Zamyka, gdy żadnego z tych
powodów już nie ma (§3.6 kontraktu, decyzja 54, progi w §10).

### Stan

```bash
./bin/mise exec -- gh run list --workflow ci.yml --branch main --limit 5
./bin/mise exec -- gh run list --workflow ci.yml --event schedule --limit 3
./bin/mise run ci:main-health -- --dry-run   # co trafiłoby do issue, bez zmian w nim
```

### Czerwony `main`

**Objaw:** e-mail GitHuba o czerwonym biegu `ci` na `main`, po dobie issue
„Zdrowie main”. Publikacja charta czeka na zielone `cluster`, `negatives`
i `tools`, więc staging stoi na ostatniej dobrej wersji. To zamrożenie jest
zamierzone — nie omijaj go ręczną publikacją.

```bash
./bin/mise exec -- gh run view <id biegu> --log-failed
```

1. **Revert przed poprawką** — na naprawę jest doba (§3.6). Gdy winny
   jest commit, także para PR-ów zielonych osobno (bez kolejki scalania
   ich połączenie sprawdza dopiero `main`, decyzja 22):
   `git switch -c revert-<sha> origin/main && git revert <sha>`, PR
   z przyczyną w opisie. Poprawka wraca osobnym PR-em, gdy przyczyna jest
   znana.
2. **Wstrzymaj auto-merge Dependabota** — scalałby dalej na czerwony `main`:

   ```bash
   ./bin/mise exec -- gh pr list --author app/dependabot \
     --json number,autoMergeRequest --jq '.[] | select(.autoMergeRequest) | .number'
   ./bin/mise exec -- gh pr merge --disable-auto <numer>
   ```

   Do zielonego `main` scalaj tylko poprawki. Potem ponów bieg
   `dependabot-auto-merge` na tych PR-ach — włączy auto-merge tam, gdzie
   pozwala decyzja 22.
3. **Winne otoczenie, nie kod** (limit pobrań Docker Hub, awaria GitHuba
   albo GHCR): `gh run rerun <id> --failed`, przyczyna w issue. Ponowienie
   przerywa serię `ci:trial` jak porażka (decyzja 36): bramka, która
   migocze, potrzebuje przyczyny, nie kolejnych ponowień.
4. Issue zamyka się samo przy pierwszym zielonym biegu `main`.

### Noc się nie odbyła

**Objaw:** issue „Zdrowie main” z „ostatni bieg nocny … temu” albo „brak
biegu nocnego”; `ci:trial` zgłasza przerwę w serii dłuższą niż 48 h.
GitHub opóźnia albo upuszcza harmonogram (03:17 UTC) przy obciążeniu,
a w repozytorium publicznym wyłącza go po 60 dniach bez aktywności.

```bash
./bin/mise exec -- gh workflow list --all   # ci: active albo disabled_inactivity
```

- `disabled_inactivity` — włącz (autor, prawa admina):
  `./bin/mise exec -- gh workflow enable ci.yml`;
- noc ręcznie: `./bin/mise exec -- gh workflow run ci.yml --ref main`.
  `workflow_dispatch` biegnie jak noc: W1 i W2, raport podatności
  z wygasającymi wyjątkami, dryf ustawień, budżety, rolling update,
  upgrade N-1 → N i rollback pod ruchem oraz negatywy na klastrze.

Bieg ręczny daje nocne sprawdzenia, ale nie dowodzi, że harmonogram działa.
`main-health` i `ci:trial` liczą tylko biegi `schedule`, więc issue zamknie
dopiero kolejna noc z harmonogramu.

## Klucz aplikacji auto-merge

Aplikacja GitHuba scala łatki od Dependabota, bo scalenie przez
`GITHUB_TOKEN` nie uruchomiłoby CI na `main` (decyzja 22). Jej klucz
prywatny to jedyny długo żyjący sekret w CI: daje token z `contents: write`
i `pull-requests: write`. Datę rotacji trzyma wpis w
`ci/contract-exceptions.yaml` (§6.5). Tydzień przed nią przypomina issue
„Zdrowie main”, po niej `dft check contract` jest czerwony.

### Rotacja (najdalej co 90 dni)

1. Ustawienia aplikacji (Settings → Developer settings → GitHub Apps →
   aplikacja → Private keys) → *Generate a private key*. Stary klucz
   zostaje do kroku 4.
2. Nowy klucz jako sekret Dependabota, potem usuń plik:

   ```bash
   ./bin/mise exec -- gh secret set DOCFIND_AUTOMERGE_PRIVATE_KEY --app dependabot < nowy.pem
   shred -u nowy.pem
   ```

3. Sprawdź na otwartym PR-ze Dependabota:
   `./bin/mise exec -- gh run list --workflow dependabot-auto-merge.yml --limit 3`,
   potem `gh run rerun <id>`. Krok tokenu aplikacji ma przejść bez błędu.
4. Usuń stary klucz w ustawieniach aplikacji.
5. PR z nową datą w `ci/contract-exceptions.yaml` (najdalej 90 dni od
   rotacji) i wpisem w DECYZJE. Rejestr wyjątków to pole chronione — bez
   wpisu `dft check contract` odrzuci zmianę.

### Pilnie — klucz wyciekł

Od razu usuń klucz w ustawieniach aplikacji. Auto-merge staje, ale nic
innego od klucza nie zależy. Potem rotacja od kroku 1. Sprawdź, co
aplikacja scaliła od chwili wycieku:

```bash
./bin/mise exec -- gh pr list --state merged --search "merged:>=<data wycieku>" \
  --json number,author,mergedBy --jq '.[] | "\(.number) \(.author.login) \(.mergedBy.login)"'
```

PR scalony przez aplikację (`mergedBy` — `app/…`), którego autorem nie jest
`app/dependabot`, to nadużycie: revert i przegląd zmian.

