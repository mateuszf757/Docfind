# Kontrakty inżynierskie DOCFIND

Wersja 2.0 · obowiązuje od scalenia PR-a, który ją wprowadza · właściciel: autor repozytorium ·
sprawdza: `dft check contract` (część `dft test`) · zmiany: historia tego pliku i wpisy
w `docs/DECYZJE.md`, od decyzji 50.

## 0. Jak czytać i zmieniać ten dokument

**0.1 Czym jest.** Kontrakt to obietnica producenta wobec konsumenta z progiem, strażnikiem
i dowodem. Każda klauzula poza §0 ma pola: Egzekwuje, Dowód, Warstwa, Wariant negatywny,
Decyzja. Część klauzuli, której strażnika jeszcze nie ma, stoi w znaczniku [ŻYCZENIE: czego
brakuje]. Strażnik i decyzja w znaczniku to plan. Poza znacznikiem to odwołanie, które musi
istnieć: zadanie mise, polecenie albo bramka `dft`, pakiet narzędzi, numer decyzji.

**0.2 Pierwszeństwo.** Ten dokument mówi, co obiecujemy i jak to jest sprawdzane;
`docs/DECYZJE.md` — dlaczego i za jaką cenę. `dft check contract` łapie tabelę progów różną od
kodu, klauzulę bez pól i odwołanie do strażnika albo decyzji, których nie ma. Rozjazd treści
klauzuli z zachowaniem strażnika naprawia jeden PR, który zmienia klauzulę, decyzję i strażnika;
do tego czasu obowiązuje zachowanie strażnika.

**0.3 Zmiana.** PR zmieniający klauzulę zmienia też wpis w DECYZJE (co wybieram, co tracę,
kiedy zmieniam zdanie), strażnika i jego wariant negatywny. Zmiana pola chronionego bez zmiany
`docs/DECYZJE.md` w tym samym PR-ze jest czerwona (§8.3). Próg zmienia się w kodzie
(`core/contract`), a tabelę w §10 przepisuje `ci/dft contract thresholds --write` — nigdy
odwrotnie.

**0.4 Wyjątek.** Wyjątek od klauzuli to wpis w DECYZJE z uzasadnieniem i datą wygaśnięcia nie
dalej niż 90 dni [ŻYCZENIE: rejestr `ci/contract-exceptions.yaml` z walidacją dat i ostrzeżeniem
nocy 7 dni przed wygaśnięciem]. Wyjątki podatności: `ci/vuln-exceptions.yaml` — uzasadnienie
i data najdalej za 90 dni, sprawdzane przez `core/vuln`. Wyjątki skanera sekretów:
`ci/gitleaks-ignore` — pole chronione (§8.3), bo nie wygasają.

**0.5 Pomiary.**
- Bieg — pierwsze podejście; ponowienie jest porażką (decyzja 36).
- Pominięte to nie zaliczone — zadanie za warunkowym przodkiem sprawdza wyniki jawnie
  (decyzja 47).
- Wall-clock warstwy — `max(completed_at)` zadań warstwy minus `created_at` biegu.
- Budżet — p95 z 20 ostatnich biegów danego zdarzenia; przekroczenie w ≥ 3 z 20 → ostrzeżenie
  w podsumowaniu, w ≥ 5 → czerwona noc [ŻYCZENIE: pomiar budżetów].
- Kolejka — `started_at` pierwszego zadania minus `created_at` biegu, raportowana osobno.
- Czas w bramkach — zegar monotoniczny; statystyka — maksimum w biegu, chyba że klauzula mówi
  inaczej.

**0.6 Słownik.**
- Tożsamość artefaktu — obraz po digeście, commit z etykiety obrazu i `DOCFIND_VERSION` na
  każdym podzie.
- Tożsamość workload — token konta usługi z określonym odbiorcą użyty do uwierzytelnienia
  w OpenBao.
- Gotowość — sonda gotowości; niegotowy pod nie dostaje ruchu.
- Wariant negatywny — wejście, które bramka musi odrzucić, na prawdziwym obrazie albo klastrze.
- Atestacja — podpisane oświadczenie in-toto w formacie bundle Sigstore; „podpis obrazu” znaczy
  atestację.
- Pakiet dowodów — atestacja przy digeście wydania z wynikami bramek (§7.1).
- Środowisko efemeryczne — klaster tworzony i usuwany w biegu (`ci`).
- Przestój — najdłuższa przerwa między kolejnymi udanymi odpowiedziami w pętli przez Gateway.
- Wejście — plik powłoki ≤ 15 linii bez komentarzy, bez pętli, parsowania i czekania,
  uruchamiający program albo `mise run`.
- Decyzja o wyniku — logika, od której zależy, czy bramka jest zielona.
- Commit bazowy — na PR-ze `pull_request.base.sha`; lokalnie podany w `--base`.

## 1. Narzędzia i wejścia

**1.1 Logika w Go albo Pythonie.** Decyzja o wyniku bramki, pomiar, analiza logów i odczyty
API — w `tools/` (Go); Python tylko przy kodzie aplikacji. W `ci/` i `bin/` wyłącznie wejścia.
Egzekwuje: przegląd PR-a; shellcheck w `dft check workflows` (poprawność, nie rozmiar)
[ŻYCZENIE: lint wejść — rozmiar, pętle, parsowanie, czekanie]. Dowód: log `test`. Warstwa: W1.
Wariant negatywny: [ŻYCZENIE: skrypt z pętlą albo `jq` w `ci/` → czerwony `test`].
Decyzja: 23.

**1.2 Launcher mise.** `bin/mise` to wygenerowany skrypt startowy mise z przypiętą wersją
i sumami, nieedytowany ręcznie; narzędzia instaluje z `mise.lock`.
Egzekwuje: `dft check versions` (spójność wersji narzędzi) [ŻYCZENIE: wersja uruchomionego mise
równa wersji z `bin/mise`; nadpisania `MISE_VERSION` i `MISE_INSTALL_PATH` w CI jako błąd].
Dowód: log `test`. Warstwa: W1. Wariant negatywny: [ŻYCZENIE: `MISE_INSTALL_PATH` wskazujący
inny plik → kod 2]. Decyzja: 23.

**1.3 Workflowy.** Dozwolone w YAML-u: wyzwalacze, graf `needs`, warunki zdarzeń i wyników,
klasyfikacja bez uruchamiania kodu z repozytorium (decyzja 22). Zakazane: wybór trybu bramki
w `run:` (tryb wybiera `dft` z argumentów, np. `dft test --event`), zadanie bez funkcji statusu
w warunku za przodkiem, który bywa pominięty, prawo zapisu w zadaniu osiągalnym z PR-a (§2.3).
Egzekwuje: `dft check workflows` — reguła grafu (decyzja 47) i uprawnień (decyzja 49)
[ŻYCZENIE: reguła odrzucająca warunek powłoki w `run:`]. Dowód: log `test`. Warstwa: W1.
Wariant negatywny: `promote` z `if: github.ref_type == 'tag'` za pominiętym `build` → czerwony
`test` (testy `core/workflow`). Decyzja: 22, 47.

**1.4 Jedno wejście.** Każdy krok, który rozstrzyga wynik bramki, jest zadaniem mise
i uruchamia się lokalnie tym samym poleceniem co w CI.
Egzekwuje: przegląd PR-a; pola chronione (§8.3) [ŻYCZENIE: reguła w `dft check workflows` —
krok `run:` woła wyłącznie `./bin/mise` albo akcję]. Dowód: log `test`. Warstwa: W1.
Wariant negatywny: [ŻYCZENIE: krok `run: go test ./...` w workflowie → czerwony `test`].
Decyzja: 23.

## 2. Zdarzenia, środowiska, promocja

**2.1 Model.** Trunk-based z promocją niezmiennego artefaktu. Jedyna gałąź długo żyjąca: `main`.

| Zdarzenie | Środowisko | Artefakt | Kto zatwierdza |
|---|---|---|---|
| PR do `main` | `ci` (efemeryczny) | archiwum `docker save` z `build`, sprawdzane digestem konfiguracji | wymagane statusy (§3.2) |
| push na `main` | `ci` (efemeryczny), potem `staging` | obraz z GHCR po digeście z `build-publish`; chart OCI po zielonym `cluster` | wymagane statusy na PR-ze; staging — Argo CD z zakresem wersji |
| tag `vX.Y.Z` (z adnotacją z `dft release`) | `prod` | ten sam digest z `main`, przyjęty na stagingu | autor w Environment `production` |

Egzekwuje: definicje środowisk (`core/environment`), Environment `production` (tylko tagi
`v*`, recenzent), `stages/admission`, `stages/promote`. Dowód: atestacje, GitHub Deployment.
Warstwa: W3, W5. Wariant negatywny: obraz z `main` bez atestacji wydania → `dft promote` kod 1.
Decyzja: 35, 38, 40, 45, 46.

**2.2 Prod tylko z wydania.** Na prod trafia wyłącznie digest z pochodzeniem z
`refs/heads/main` i atestacją wydania z tagu `vX.Y.Z`, po zgodzie autora; każda promocja zostawia
Deployment.
Egzekwuje: `dft promote`; zadanie `promote` biegnie tylko po jawnym sukcesie `release`,
`attest-release` i `vulns` (decyzja 47) [ŻYCZENIE: ćwiczenie promocji w repozytorium próbnym
przed pierwszym wydaniem — procedura w `docs/RUNBOOK.md`, wykonuje autor].
Dowód: Deployment `production` [ŻYCZENIE: pakiet dowodów, §7.1]. Warstwa: W5.
Wariant negatywny: środowisko bez wymaganej zgody → `dft promote` kod 1. Decyzja: 38, 40, 47.

**2.3 Nic z PR-a nie dociera do środowiska długo żyjącego.** Zadania osiągalne z
`pull_request` nie mają tokenu z prawem zapisu: ani `packages`, `id-token`, `attestations`,
`issues`, `contents`, ani żadnego innego zakresu.
Egzekwuje: reguła uprawnień w `dft check workflows` (decyzja 49) [ŻYCZENIE: noc sprawdza
nagłówek „GITHUB_TOKEN Permissions” ostatniego PR-a przez API]. Dowód: log `test`. Warstwa: W1.
Wariant negatywny: `packages: write` w zadaniu osiągalnym z PR-a → czerwony `test` (testy
`core/workflow`). Decyzja: 49.

**2.4 Parytet stagingu z prod.** Render `staging` i `prod` różni się wyłącznie polami z listy
dozwolonych różnic: host, wydawca, strefa ACME, porty, źródła artefaktu, sposób dostawy.
Repliki, PDB, NetworkPolicy, limity, wersje komponentów — identyczne.
Egzekwuje: [ŻYCZENIE: diff renderów chart × środowisko w `dft test`]; dziś każdy render jest
walidowany (`dft check charts`), ale nie porównywany z innymi. Dowód: log `test`. Warstwa: W1.
Wariant negatywny: [ŻYCZENIE: `api.replicas: 1` tylko w `staging` → czerwony `test`].
Decyzja: 35.

**2.5 Akceptacja wydania.** `dft release` uruchamia bramki akceptacji na stagingu na celu
zapisanym na starcie — obraz Deploymentu i wersja charta z Application Argo CD — i porównuje go
po każdej bramce. Kod bramek pochodzi z zatwierdzonego commita, który jest przodkiem
`origin/main`; tag zapisuje go jako `Gates-Commit:`.
Egzekwuje: `stages/release`. Dowód: adnotacja tagu (`Gates-Commit:`, wersja charta)
[ŻYCZENIE: pakiet dowodów, §7.1]. Warstwa: W5 (u autora). Wariant negatywny: zmiana obrazu albo
wersji charta w trakcie → kod 1, bez tagu; kod bramek z niezatwierdzonymi zmianami albo spoza
`origin/main` → kod 1 (testy `stages/release`). Decyzja: 40, 48.

## 3. Warstwy CI i budżety

**3.1 Warstwy.** Każde zadanie CI należy do jednej warstwy.

| Warstwa | Zdarzenia | Zadania i zakres | Budżet (p95, §0.5) | Wymagana |
|---|---|---|---|---|
| W1 szybka | PR, push `main`, noc | równolegle: `test` — wersje, lint, kontrakt, gitleaks na historii, Python, charty z politykami, Go z `-race`, govulncheck; `build` albo `build-publish` — obraz, baza, testy na musl, warunki Etapu 1, powtarzalność; `negatives` — warianty negatywne; `tools` — powtarzalność narzędzi | 6 min [ŻYCZENIE: zmierzone 2026-10-15, przed zrównolegleniem — 15,3 min] | tak: `fast-feedback` |
| W2 integracja przed scaleniem | PR, push `main`, noc | po `test` i `build`: `cluster` — klaster `ci`, wdrożenie, tożsamość, drain każdego węzła, OpenBao, rotacja, TLS, dowody; `vulns` — skan obrazu | 20 min [ŻYCZENIE: zmierzone 2026-10-15 — 23,7 min] | tak: `integration` |
| W3 po scaleniu | push `main` | `attest`, `chart`, `attest-chart`: pochodzenie i SBOM, atestacja, chart OCI z digestem przetestowanym | 30 min (2026-10-15: 24,1 min) | nie (czerwony → §3.6) |
| W4 noc | `schedule`, `workflow_dispatch` | W1 i W2 na `main`; `vulns-issue` z wyjątkami wygasającymi w ciągu tygodnia; `settings-drift`; `main-health` (także na push) [ŻYCZENIE: negatywy na klastrze, upgrade N-1 → N] | 40 min (2026-10-15: 23,9 min) | nie |
| W5 wydanie | tag `vX.Y.Z` | `release`, `attest-release`, `vulns` z polityką wydania, `promote` po zgodzie | 10 min do oczekiwania na zgodę (2026-10-15: 2,3 min) | nie (zatwierdza autor) |

Egzekwuje: [ŻYCZENIE: `dft check workflows` — zadanie bez znacznika warstwy to błąd;
`dft ci budget` — budżety z 20 biegów]. Dowód: [ŻYCZENIE: podsumowanie nocy z p50 i p95 na
zadanie i zdarzenie]. Warstwa: wszystkie. Wariant negatywny: [ŻYCZENIE: zadanie bez warstwy →
czerwony `test`; `sleep 400` w kroku W1 → przekroczenie zgłoszone]. Decyzja: 36.

**3.2 Wymagane statusy.** Reguła `main` (`.github/rulesets/main.json`) wymaga dokładnie
`fast-feedback` i `integration` z aplikacji GitHub Actions. Oba to zadania zbierające
z `always()`: biegną także wtedy, gdy zadanie warstwy zawiodło, zostało anulowane albo
pominięte, i przechodzą tylko wtedy, gdy każde zadanie warstwy ma wynik success
(`ci:require`). Pominięte zadanie wymagane raportuje sukces, więc żadne zadanie, które
wykonuje bramkę, nie jest wymaganym statusem.
Egzekwuje: `dft check workflows` — wymagany status to zadanie workflowu na pull_request
z `always()` w warunku; `ci:require`; ruleset stosowany przez `dft repo-settings`; pola
chronione (§8.3); noc porównuje reguły działające na `main` z plikiem
(`dft repo-settings --check`, §6.7). Dowód: efektywne reguły
`GET /repos/…/rules/branches/main`, log zadań zbierających. Warstwa: W1, W2.
Wariant negatywny: wynik skipped w `NEEDS` → `ci:require` kod 1; wymagany status bez
`always()` albo bez zadania → czerwony `test` (testy `core/workflow`, `cmd/dft`).
Decyzja: 22, 36, 38, 47, 52.

**3.3 Okres próbny.** Nowa bramka wchodzi do wymaganych po 10 kolejnych biegach na `main`
(push i noc) za pierwszym podejściem. Zadanie, którego tryb na PR-ze różni się od trybu na
`main` (`vulns`: na PR-ze blokuje względem obrazu bazowego, na `main` raportuje), ma drugą
serię: 10 kolejnych scalonych PR-ów, na których ostatnim commicie przeszło za pierwszym
podejściem.
Egzekwuje: `dft trial` (`ci:trial`, flaga `--pull-requests`); przerwa między biegami nocnymi
dłuższa niż próg z §10 przerywa serię. Dowód: wyjście `ci:trial`. Warstwa: W4 (pomiar), dotyczy
W1–W2. Wariant negatywny: bieg ponowiony przerywa serię; PR scalony z czerwonym zadaniem
przerywa serię PR-ów; dwie noce bez biegu przerywają serię (testy `stages/trial`).
Decyzja: 36, 51, 54.

**3.4 Bez automatycznych ponowień.** Ani workflow, ani `dft` nie ponawia nieudanej bramki;
ponawianie wywołań zewnętrznych (429, rejestr) tylko w adapterze, z limitem i logiem.
Egzekwuje: `dft trial` (ponowienie to porażka); pola chronione (§8.3) [ŻYCZENIE: reguła
w `dft check workflows` — brak akcji ponawiających]. Dowód: `run_attempt` w API.
Warstwa: wszystkie. Wariant negatywny: bieg z `run_attempt` 2 przerywa serię `dft trial`.
Decyzja: 36.

**3.5 Feedback.** Czas do pierwszego czerwonego sygnału na PR-ze ≤ budżet W1; warstwa W2 nie
opóźnia sygnału W1.
Egzekwuje: graf zadań — zadania W1 bez needs między sobą, W2 po `test` i `build`; pola
chronione (§8.3) [ŻYCZENIE: `dft ci budget` mierzy czas do pierwszego czerwonego sygnału].
Dowód: [ŻYCZENIE: podsumowanie nocy]. Warstwa: W1, W2. Wariant negatywny: [ŻYCZENIE: zadanie
W2 przed zadaniem W1 w grafie → reguła grafu zgłasza]. Decyzja: 36, 52.

**3.6 Czerwony `main`.** Właściciel naprawia w ≤ 24 h (revert przed poprawką); w tym czasie
scalane są tylko poprawki, auto-merge Dependabota jest wstrzymany, staging świadomie zamrożony.
Egzekwuje: graf zadań — `chart` publikuje się tylko po zielonych `cluster`, `negatives`
i `tools`, więc czerwony `main` zamraża staging; `ci:main-health` (zadanie `main-health` na
push i w nocy) prowadzi issue, gdy `main` jest czerwony dłużej niż próg z §10 albo noc nie
przyszła [ŻYCZENIE: wstrzymanie auto-merge Dependabota przy czerwonym `main`]. Dowód: issue
„Zdrowie main”. Warstwa: W3, W4. Wariant negatywny: `main` czerwony od 50 h → issue; czerwony
od godziny → bez issue (testy `stages/mainhealth`). Decyzja: 22, 36, 45, 54.

## 4. Artefakty

**4.1 Obraz.** Jeden build na commit; obraz niesie commit (`version.json`, etykiety OCI), nie
wersję; publikacja z pochodzeniem SLSA i SBOM z przypiętego generatora; na PR-ze i nocą
archiwum z digestem konfiguracji; użytkownik numeryczny różny od 0.
Egzekwuje: `dft build` (weryfikacja w rejestrze po publikacji), `check:base`, `check:runtime`
[ŻYCZENIE: USER różny od 0 sprawdzany w `check:runtime`]. Dowód: digest w wyjściach
`build-publish`, atestacje w GHCR. Warstwa: W1, W3. Wariant negatywny: obraz w rejestrze bez
indeksu albo atestacji → `dft check published` odmawia [ŻYCZENIE: USER root → NIESPEŁNIONE].
Decyzja: 21, 37, 40.

**4.2 Downstream nie buduje produktu.** Zadania po buildzie konsumują archiwum albo digest;
chart instalowany w W2 i W3 to ten sam plik, który publikuje `chart`.
Egzekwuje: digest konfiguracji archiwum w `cluster` i `vulns` [ŻYCZENIE: porównanie digestu
charta przetestowanego z publikowanym]. Dowód: wyjścia zadań. Warstwa: W2, W3.
Wariant negatywny: podmienione archiwum → `cluster` kod 1 [ŻYCZENIE: chart w rejestrze o innym
digeście niż przetestowany → `chart` kod 1]. Decyzja: 36, 45.

**4.3 Klaster tylko po zielonym buildzie.** `cluster` biegnie po sukcesie `test` i `build`
(PR, noc) albo `build-publish` i `attest` (push).
Egzekwuje: warunek zadania `cluster` w `ci.yml` — pole chronione (§8.3). Dowód: graf biegu.
Warstwa: W2. Wariant negatywny: zmiana warunku bez wpisu w DECYZJE → czerwony `test`.
Decyzja: 36, 47, 49.

**4.4 Rozmiar obrazu.** Obraz rozpakowany mieści się w progu z §10.
Egzekwuje: `check:runtime` (`stages/lifecycle`) [ŻYCZENIE: rozmiar liczony z manifestu
i warstw, niezależnie od magazynu Dockera — dziś miara zależy od magazynu]. Dowód:
`runtime-api.json`. Warstwa: W1. Wariant negatywny: `--max-image-bytes` poniżej rozmiaru →
NIESPEŁNIONE. Decyzja: 14.

## 5. Kontrakty behawioralne

**5.1 Zamykanie i żądania w locie (obraz).** Koszt własny zamykania ponad narzut
`docker stop`, żądanie w locie przy SIGTERM krótsze niż `shutdown_grace_seconds` zakończone
odpowiedzią 200, odmowa startu bez konfiguracji i ze złą konfiguracją (`BŁĄD KONFIGURACJI`
z nazwą pola) — progi w §10.
Egzekwuje: `check:runtime`, `test:negatives` (`stages/lifecycle`). Dowód: `runtime-api.json`
(artefakt). Warstwa: W1. Wariant negatywny: PID 1 bez obsługi SIGTERM, żądanie dłuższe niż
grace, obraz, który bez konfiguracji wstaje — każdy odrzucony i każdy w budżecie wariantu
(§10). Decyzja: 32.

**5.2 Drain i rolling update.** Drain każdego węzła z repliką: minimum żądań na drain,
0 odpowiedzi innych niż 200 w pętli do Service, czas `kubectl drain` — progi w §10. Okres łaski
poda pokrywa X (§10), co sprawdza render charta.
Egzekwuje: `cluster:drain` (`stages/drain`); relacja X — `fail` w szablonie charta
[ŻYCZENIE: pomiar zakończenia każdego eksmitowanego poda w ≤ X bez SIGKILL, kod wyjścia różny
od 137; pętla przez Gateway]. Dowód: `drain-<env>.json`, `drain-probe-*.jsonl`. Warstwa: W2;
staging przy wydaniu. Wariant negatywny: okres łaski krótszy niż X → render odrzucony
[ŻYCZENIE: opóźnienie zaślepki 5 s przy grace 3 s → drain czerwony]. Decyzja: 6, 32, 34.

**5.3 Gotowość.** Niegotowy pod nie dostaje żądań; `/readyz` nie zależy od zależności wspólnych
dla wszystkich replik (od Etapu 6).
Egzekwuje: [ŻYCZENIE: bramka gotowości przed Etapem 6]. Dowód: [ŻYCZENIE: raport bramki].
Warstwa: W2. Wariant negatywny: [ŻYCZENIE: pod z wymuszonym 503 na `/readyz` dostaje 0 żądań
przez 30 s]. Decyzja: [ŻYCZENIE: nowa, przed Etapem 6].

**5.4 Odnowienie certyfikatu pod ruchem.** Pętla HTTPS z nowym połączeniem na każde żądanie
w trakcie odnowienia: minimum żądań, 0 nieudanych, 0 restartów proxy; proxy podaje nowy numer
seryjny w czasie Y od odnowienia — progi w §10.
Egzekwuje: `cluster:tls` (`stages/ingress`) — pierwsza obserwacja nowego certyfikatu
[ŻYCZENIE: każda replika proxy, potwierdzona 20 kolejnymi pełnymi handshake'ami do każdego
poda; proxy na co najmniej 2 węzłach]. Dowód: `tls-<env>.json`. Warstwa: W2; staging przy
wydaniu [ŻYCZENIE: prod — obserwacja naturalnego odnowienia i alarm na wiek certyfikatu].
Wariant negatywny: [ŻYCZENIE: replika odcięta od xDS → czerwony]. Decyzja: 4, 11.

**5.5 Rotacja sekretu.** Nowy token dociera do każdego poda bez restartu: faza „nowy obok
starego” i „tylko nowy”, każda w budżecie z §10, te same UID podów i liczba restartów.
Egzekwuje: `cluster:rotation` (`stages/rotation`). Dowód: `rotation-<env>.json`. Warstwa: W2.
Wariant negatywny: pod, do którego rotacja nie dociera, otwarte `/metrics` → bramka zatrzymuje
się na punkcie wyjścia. Decyzja: 44.

**5.6 Samonaprawa.** Replika ponad chart i usunięty PDB wracają do stanu z charta w budżecie
z §10, a Application jest `Synced`.
Egzekwuje: `cluster:selfheal` (`stages/selfheal`) [ŻYCZENIE: pętla ruchu i przestój ≤ Z, jak
w §5.7]. Dowód: `selfheal-staging.json`. Warstwa: staging. Wariant negatywny: Application bez
samonaprawy — zmiana nie wraca w budżecie → NIESPEŁNIONE. Decyzja: 46.

**5.7 Upgrade bez przestoju.** `helm upgrade` z N-1 do N i synchronizacja Argo CD: przestój
≤ Z = 1 s, 0 nieudanych żądań w pętli 15–20 żądań/s przez Gateway (w drainach zmierzono
najdłuższą przerwę 0,17–0,25 s); rollback przez przypięcie wersji ≤ 15 min od decyzji do
`Synced`.
Egzekwuje: [ŻYCZENIE: `stages/upgrade` w nocy i na stagingu]. Dowód: [ŻYCZENIE: raport
`upgrade-<env>.json`]. Warstwa: W4, staging. Wariant negatywny: [ŻYCZENIE: chart N
z opóźnieniem zaślepki dłuższym niż grace → czerwony]. Decyzja: 13, 45, 46.

**5.8 Tożsamość artefaktu.** Każdy pod aplikacji ma obraz z Deploymentu, commit z obrazu
i `DOCFIND_VERSION` z Deploymentu; liczba podów = repliki.
Egzekwuje: `cluster:identity` (`stages/identity`), koniec `dft cluster`. Dowód:
`identity-<env>.json`. Warstwa: W2, staging. Wariant negatywny: obraz z innego commita →
kod 1. Decyzja: 30, 40.

**5.9 Tożsamość workload łańcucha sekretów.** Sekrety aplikacji pobiera ESO kontem
`docfind-secrets` tokenem z odbiorcą `openbao`, rolą na jeden sekret; pod aplikacji nie ma
tokenu konta usługi; mTLS — dopiero dla ruchu między usługami poza proxy (Etap 6),
z wewnętrznego CA.
Egzekwuje: konfiguracja ESO i OpenBao z chartów platformy, `cluster:rotation` (sekret dociera
tylko tą drogą) [ŻYCZENIE: reguła „pod aplikacji bez tokenu” w `core/policy`; negatywy w nocy —
zły odbiorca, obce konto, wygasły token]. Dowód: log, audyt OpenBao. Warstwa: W2
[ŻYCZENIE: W1, W4]. Wariant negatywny: [ŻYCZENIE: automountServiceAccountToken true w podzie
aplikacji → czerwony `test`]. Decyzja: 42, 43, 44.

**5.10 Powtarzalność.** Build z pamięcią podręczną i od zera daje identyczne bajty: obraz
i binarki narzędzi (pusty `GOCACHE`).
Egzekwuje: `check:reproducible`, `check:tools`. Dowód: sumy w podsumowaniu biegu.
Warstwa: W1. Wariant negatywny: plik z bieżącą datą w warstwie → różnica wykryta.
Decyzja: 21, 23.

## 6. Jakość i bezpieczeństwo

**6.1 Pokrycie.** Go: per pakiet warstwy stages ≥ 70%, core ≥ 70%, adapters ≥ 55%,
kit ≥ 60% (minimum z pomiaru 2026-10-15 zaokrąglone w dół do 5 pp); cały moduł nie spada
o więcej niż 0,5 pp względem commita bazowego; cmd — tylko zapadka. Python: próg = pierwszy
pomiar − 5 pp.
Egzekwuje: [ŻYCZENIE: zapadka pokrycia w `dft check go`]. Dowód: [ŻYCZENIE:
`coverage-go.json`]. Warstwa: W1. Wariant negatywny: [ŻYCZENIE: PR usuwający test etapu →
czerwony]. Decyzja: 39.

**6.2 Podatności.** Blokuje podatność naprawialna o wadze HIGH, CRITICAL albo nieznanej, bez
ważnego wyjątku (§10): na PR-ze — tylko nowa względem obrazu i kodu `tools/` z commita
bazowego; gdy obrazu commita bazowego jeszcze nie ma w rejestrze — względem obrazu
najbliższego przodka (§10), z ostrzeżeniem w raporcie; na tagu — każda; na `main` i nocą —
raport i issue.
Egzekwuje: `check:vulns` (`stages/vulns`), govulncheck w `dft check go` i `dft test`.
Dowód: `vulns-api.json` (punkt odniesienia i obraz, którego zabrakło), `vulns-go.json`
z wersjami baz. Warstwa: W1 (Go), W2 (obraz), W5. Wariant negatywny: wygasły wyjątek →
blokuje; rejestr bez obrazu commita bazowego → porównanie z obrazem przodka, a bez żadnego
obrazu — blokuje każda naprawialna (testy `stages/vulns`). Decyzja: 37, 51.

**6.3 Sekrety w gicie.** Skan całej historii sprawdzanego commita z diffem scaleń;
konfiguracja i wyjątki tylko z `ci/`, znacznik wyciszenia w kodzie nie działa; znalezisko =
kod 7.
Egzekwuje: `check:secrets` w `dft test`; push protection GitHuba. Dowód: `gitleaks.json`
(z redakcją). Warstwa: W1. Wariant negatywny: repozytorium-przynęta z `.gitleaks.toml`
i `gitleaks:allow` → znalezisko. Decyzja: 41.

**6.4 Pody.** Nasze charty, przy renderze: requests CPU i pamięci, limit pamięci, bez limitu
CPU, obrazy z zewnątrz po digeście, sekrety jako wolumen bez subPath. Przestrzenie `docfind`,
`openbao`, `argocd` z PSA `restricted` — runAsNonRoot, drop ALL, bez eskalacji uprawnień,
seccomp `RuntimeDefault` — w API serverze.
Egzekwuje: `core/policy` w `dft check charts`; PSA w API serverze, `test:cluster` lokalnie
[ŻYCZENIE: w `core/policy` także readOnlyRootFilesystem, capabilities
i automountServiceAccountToken false dla podów aplikacji; `test:cluster` w nocy]. Dowód: log
`test`, log `test:cluster`. Warstwa: W1 (render) [ŻYCZENIE: W4 (klaster)]. Wariant negatywny:
limit CPU w charcie → czerwony `test`; sonda bez `securityContext` → odrzucona przez API
server. Decyzja: 16, 20, 31, 44.

**6.5 Poświadczenia w CI.** Tylko `GITHUB_TOKEN` z uprawnieniami na zadanie i OIDC
w zadaniach atestacji; `persist-credentials: false`; bez `pull_request_target`. Jedyny wyjątek:
klucz aplikacji auto-merge w sekretach Dependabota.
Egzekwuje: zizmor i reguła uprawnień w `dft check workflows` (decyzja 49) [ŻYCZENIE: data
rotacji klucza aplikacji w rejestrze wyjątków, najdalej za 90 dni]. Dowód: log `test`.
Warstwa: W1. Wariant negatywny: prawo zapisu w zadaniu osiągalnym z PR-a → czerwony `test`.
Decyzja: 22, 49.

**6.6 Łańcuch dostaw.** Wszystko z zewnątrz niezmiennie: akcje pełnym SHA (wymuszone przez
GitHuba), obrazy digestem, narzędzia z `mise.lock` (`locked = true`), charty sumą, moduły Go
z `go.sum` i tylko z `proxy.golang.org`; cooldown Dependabota 7 dni (major 14).
Egzekwuje: mise, ustawienie `sha_pinning_required` (`dft repo-settings`), `core/policy`
(digest), `dft check versions` [ŻYCZENIE: obrazy systemowe k3s z archiwum airgap z sumą;
nadpisania z otoczenia w CI — `MISE_*`, `GOPROXY`, `GOFLAGS`, `GONOSUMDB`, `GOINSECURE` — jako
błąd]. Dowód: log `test` [ŻYCZENIE: lista obrazów z klastra `ci`]. Warstwa: W1, W2.
Wariant negatywny: obraz z zewnątrz bez `@sha256:` w charcie → czerwony `test`.
Decyzja: 20, 22, 23.

**6.7 Ustawienia repozytorium.** Reguły `main` i tagów, środowiska, auto-merge, push
protection i wymóg SHA — jako kod w `.github/`, stosowane przez `dft repo-settings`; zmiana
plików to zmiana kontraktu (§8.3).
Egzekwuje: `repo:settings`, pola chronione (§8.3), w nocy `dft repo-settings --check`
(zadanie `settings-drift`): reguły działające na `main` i środowiska — czy wymagają zgody,
skąd wolno wdrażać [ŻYCZENIE: auto-merge, push protection, wymóg SHA i retencja w porównaniu —
ich odczyt wymaga uprawnień administratora]. Dowód: log `test`, log nocy. Warstwa: W1 (pliki),
W4 (stan). Wariant negatywny: zmiana pliku reguły bez wpisu w DECYZJE → czerwony `test`;
wymagany status albo zgoda zmienione w UI → noc czerwona (testy `stages/reposync`).
Decyzja: 22, 38, 54.

## 7. Dowody i retencja

**7.1 Pakiet dowodów wydania.** Przy digeście każdego wydania atestacja pakietu dowodów
z predykatem `…/attestations/evidence/v1`: zapis wydania (digest, commit, akceptacja na
stagingu z `Gates-Commit`); biegi `ci` po push na `main` dla commita wydania z wynikami
wszystkich zadań i skrótami SHA-256 artefaktów; wersje narzędzi (skrót `mise.lock`, commit
kodu `dft`, Go). Ostatni bieg `main` commita wydania jest zielony, a `test`, `build-publish`,
`attest`, `cluster` i `chart` mają w nim wynik success — inaczej wydanie nie dostaje numeru
w rejestrze.
Egzekwuje: `dft release-image` (`stages/release`, przed tagiem w rejestrze), atestacja
w `attest-release`, `dft promote` (`stages/admission` wymaga pakietu z tym digestem i zielonym
biegiem) [ŻYCZENIE: raporty bramek jako artefakty OCI w referrerach digestu; raporty
akceptacji na stagingu w pakiecie; zgoda na promocję w osobnym zapisie]. Dowód: atestacja
z tym predykatem obok obrazu w GHCR i w Rekorze. Warstwa: W5. Wariant negatywny: digest
z `main` bez wydania albo wydanie bez pakietu → `dft promote` kod 1; czerwony bieg `main`
commita wydania → `dft release-image` kod 1 (testy `stages/release`, `stages/admission`).
Decyzja: 37, 40, 48, 53.

**7.2 Retencja.** Artefakty Actions (≤ 90 dni w repozytorium publicznym; biegi, checki
i statusy — od 2026-10-01 tak samo) to nośnik roboczy, nie dowód. Dowody wydań żyją
w atestacjach i GHCR tak długo jak obraz.
Egzekwuje: atestacje w GHCR przy obrazie [ŻYCZENIE: retencja repozytorium ustawiona jawnie
na 90 dni i sprawdzana nocą]. Dowód: [ŻYCZENIE: raport nocy]. Warstwa: W4.
Wariant negatywny: [ŻYCZENIE: retencja 30 dni → noc czerwona]. Decyzja: 37, 40.

## 8. Zasady ogólne i meta-kontrakty

**8.1 Wariant, który może zawieść.** Każda bramka ma wariant negatywny na prawdziwym obrazie
albo klastrze i jest deterministyczny: zawodzi zawsze, nie „zwykle”, i w budżecie (§10), a nie
po limicie czasu.
Egzekwuje: `test:negatives` (budżet wariantu w testach `stages/lifecycle`), `test:cluster`,
testy etapów [ŻYCZENIE: `dft check contract` — każda klauzula bramki wskazuje istniejący test
wariantu; w nocy kod 1 od każdego wariantu na klastrze]. Dowód: log `test:negatives`.
Warstwa: W1 [ŻYCZENIE: W4]. Wariant negatywny: wariant zaliczony po limicie bramki → czerwony
test. Decyzja: 32, 36.

**8.2 Dowód z systemu wykonującego.** Twierdzenie o artefakcie, ustawieniu albo konfiguracji
usługi sprawdza program w miejscu docelowym (rejestr, API server, efektywne reguły GitHuba,
przyjęta konfiguracja Dependabota), a nie walidator słabszy od systemu, który decyduje.
Egzekwuje: weryfikacja w rejestrze po publikacji (`dft build`, `chart:publish`), tożsamość na
podach (`cluster:identity`), PSA w API serverze (`test:cluster`) [ŻYCZENIE: noc porównująca
efektywne ustawienia GitHuba]. Dowód: raporty bramek. Warstwa: W2, W3. Wariant negatywny:
sonda bez `securityContext` → odrzucona przez API server. Decyzja: 31, 40, 45.

**8.3 Pola chronione.** Zmiana któregoś z tych pól idzie w jednym PR-ze ze zmianą
`docs/DECYZJE.md`:
- progi bramek (`core/contract`, tabela w §10) i ten dokument;
- konfiguracja skanera sekretów i jego wyjątki (`ci/gitleaks.toml`, `ci/gitleaks-ignore`);
- reguły repozytorium, środowiska GitHuba i konfiguracja Dependabota (`.github/rulesets/`,
  `.github/environments/`, `.github/dependabot.yml`);
- działanie workflowów i akcji złożonych: warunki, zadania, `needs`, polecenia, uprawnienia,
  limity czasu, zdarzenia, nazwy zadań (nazwy statusów).

Nie zmieniają działania: wersja akcji w `uses` (podbicia Dependabota), komentarz, nazwa kroku.
Progi bramek biorą się wyłącznie z rejestru: liczba wpisana wprost w polu progu konfiguracji
bramki to błąd.
Egzekwuje: `dft check contract --base` w `dft test` na PR-ze (commit bazowy PR-a); test
`core/contract` (pola progów z rejestru). Dowód: log `test`. Warstwa: W1.
Wariant negatywny: `DrainMinRequests = 10` bez wpisu w DECYZJE → czerwony `test`;
`MinRequests: 10` wprost w konfiguracji bramki → czerwony test `core/contract`; krok bramki
z `if: false` → czerwony `test`. Decyzja: 50.

**8.4 Kody wyjścia.** 0 — wynik pozytywny, 1 — negatywny (`NIESPEŁNIONE:`), 2 — awaria
narzędzia albo złe wywołanie (`BŁĄD:`); skaner sekretów — 7. Awaria nigdy nie wygląda jak
wynik.
Egzekwuje: `kit/cli` i jego testy; bramki sprawdzają wymagane narzędzia na starcie.
Dowód: kody w logach. Warstwa: wszystkie. Wariant negatywny: brak narzędzia (np. `uv`) →
kod 2, nie 1. Decyzja: 23.

**8.5 Lokalnie jak w CI.** Ten sam program, te same wersje, ta sama miara; różnica werdyktu
lokalnie i w CI dla tego samego wejścia jest błędem bramki.
Egzekwuje: `./bin/mise run ci` (te same zadania), `dft check versions`, tryb `dft test`
wybierany w `dft` z argumentów, a nie w workflowie [ŻYCZENIE: porównanie miar lokalnych
z podsumowaniem biegu, np. rozmiaru obrazu]. Dowód: raporty `.cache/reports` i podsumowanie
biegu. Warstwa: W1. Wariant negatywny: [ŻYCZENIE: ten sam obraz z różnym rozmiarem lokalnie
i w CI → czerwone]. Decyzja: 23.

**8.6 Każda nowa funkcja CI** ma warstwę, budżet, wariant negatywny i okres próbny (§3), a jej
wprowadzenie — wpis w DECYZJE (§8.3).
Egzekwuje: pola chronione (§8.3), `dft trial` [ŻYCZENIE: znacznik warstwy przy zadaniu
w `dft check workflows`]. Dowód: log `test`, wyjście `ci:trial`. Warstwa: W1.
Wariant negatywny: nowe zadanie bez wpisu w DECYZJE → czerwony `test`. Decyzja: 36, 50.

**8.7 Daty.** Każda obietnica z datą (wyjątki, klucze, wsparcie wersji, certyfikaty) jest
w kalendarzu danych; noc ostrzega 7 dni przed datą.
Egzekwuje: walidacja dat wyjątków podatności (`core/vuln`); wyjątek wygasający w ciągu progu
z §10 trafia do issue nocnego i trzyma je otwarte (`vulns:issue`) [ŻYCZENIE: kalendarz
pozostałych dat — klucz aplikacji auto-merge, wsparcie wersji, certyfikaty]. Dowód: log `test`,
issue nocne. Warstwa: W1, W4. Wariant negatywny: wyjątek z datą dalej niż 90 dni → czerwony
`test`; wyjątek wygasający za 7 dni → issue, za 8 dni → brak (testy `core/vuln`,
`stages/vulns`). Decyzja: 37, 54.

**8.8 Czas feedbacku** jest ważniejszy niż kompletność warstwy szybkiej; kompletność przed
scaleniem zapewnia W2.
Egzekwuje: [ŻYCZENIE: `dft ci budget`, §3.5]. Dowód: [ŻYCZENIE: podsumowanie nocy].
Warstwa: W1. Wariant negatywny: [ŻYCZENIE: zadanie W2 wstawione przed W1 w grafie → reguła
grafu zgłasza]. Decyzja: 36.

## 9. Poza zakresem tej wersji

SLO i alerty objawowe (Etap 8); kontrakt danych Elasticsearcha i reindeks bez przestoju
(Etap 6); strumieniowanie i backendy LLM (Etap 7); e2e na `stub` z korpusem i weryfikacja przy
przyjęciu w klastrze (Etap 9); chart jako publiczny interfejs — zgodność, deprecjacja, macierz
Kubernetes i Helma, `docs/INSTALACJA.md`, upgrade N-1 → N u klienta, `helm rollback` < 15 min
(Etap 10); backup, odtworzenie, RPO/RTO i paczka diagnostyczna (Etap 11). Każdy z tych punktów
wchodzi do kontraktu najpóźniej w PR-ze, który zaczyna dany etap.

## 10. Progi bramek

Tabela powstaje z rejestru progów (`core/contract`, plik
`tools/internal/core/contract/thresholds.go`), z którego biorą je bramki. Klauzule odsyłają
tutaj, zamiast powtarzać liczby: liczba wpisana w dwóch miejscach rozjeżdża się z kodem. Progi
docelowe bez strażnika (budżety warstw, Z, pokrycie) stoją w klauzulach, w znaczniku
[ŻYCZENIE] albo obok niego.

<!-- progi: początek — generuje `dft contract thresholds --write`, nie edytuj ręcznie -->
| Klauzula | Próg | Wartość | Gdzie w kodzie |
|---|---|---|---|
| §4.4 | rozmiar obrazu rozpakowanego | ≤ 200000000 B | stages/lifecycle |
| §5.1 | koszt własny zamykania ponad `docker stop` | ≤ 750ms | stages/lifecycle |
| §5.1 | żądanie w locie przy SIGTERM | 1.5s, odpowiedź 200 | stages/lifecycle |
| §5.1 | odmowa startu bez konfiguracji | w ≤ 30s | stages/lifecycle |
| §5.1 | bramka na wariancie negatywnym obrazu | ≤ 2m0s | stages/lifecycle (testy z tagiem docker) |
| §5.2 | żądania na jeden drain | ≥ 100, 0 odpowiedzi innych niż 200 | cmd/dft (drain) |
| §5.2 | `kubectl drain` jednego węzła | ≤ 3m0s | cmd/dft (drain) |
| §5.2 | okres łaski poda ≥ X | X = preStopSleepSeconds + shutdown_grace_seconds + 1 s (dziś 5 + 3 + 1 = 9 s; okres łaski 15 s) | chart docfind, _helpers.tpl (render) |
| §5.4 | żądania HTTPS w trakcie odnowienia | ≥ 50, 0 nieudanych, 0 restartów proxy | cmd/dft (tls) |
| §5.4 | nowy certyfikat na proxy (Y) | ≤ 1m0s od odnowienia | stages/ingress |
| §5.5 | faza rotacji sekretu na podzie | ≤ 4m0s | cmd/dft (rotation) |
| §5.6 | samonaprawa jednej zmiany | ≤ 1m0s | cmd/dft (selfheal) |
| §6.2 | punkt odniesienia skanu obrazu na PR-ze | obraz commita bazowego albo najbliższego z 10 przodków | stages/vulns |
| §3.3 | przerwa między biegami nocnymi w serii | ≤ 48h0m0s | stages/trial, stages/mainhealth |
| §3.6 | czerwony main bez issue | ≤ 24h0m0s | stages/mainhealth |
| §6.2 | ważność wyjątku podatności | ≤ 90 dni | core/vuln |
| §8.7 | ostrzeżenie przed wygaśnięciem wyjątku | 168h0m0s przed datą, w issue nocnym | stages/vulns, stages/gocode |
<!-- progi: koniec -->

Koniec kontraktu. Zmiana: §0.3. Wyjątek: §0.4.
