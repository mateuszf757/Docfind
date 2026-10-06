# Decyzje projektowe DOCFIND

Każda decyzja ma ten sam układ: co wybieram, co przez to tracę i przy jakim warunku
zmieniam zdanie. Wpis bez kosztu i bez warunku zmiany zdania nie jest decyzją, tylko
preferencją.

## Premisa

Klient prowadzi własny klaster Kubernetes i ma łączność z internetem. My prowadzimy swój
klaster w modelu GitOps; do klienta dostarczamy chart Helma jako artefakt OCI, a obrazy
z rejestru. Klient instaluje i utrzymuje u siebie.

Pierwotna wersja projektu zakładała dostawę na maszynę bez ruchu wychodzącego. Zrezygnowałem
z tego założenia świadomie, bo celem jest opanowanie Kubernetesa i praktyk wokół niego,
a brak internetu wycinał z projektu GitOps, ACME i rejestr obrazów — czyli rdzeń tych praktyk.
**Koszt tej zmiany:** tracę całą warstwę pakowania offline i dyscyplinę diagnozy bez dostępu
do hosta, które były najbardziej wyróżniającym elementem pierwotnego pomysłu. Zostawiam z niej
jedno: paczkę diagnostyczną i cykl sesji diagnostycznych z Etapu 11.

---

## 1. Kubernetes zamiast docker compose

**Wybieram bardziej złożone.** Compose byłby wystarczający dla sześciu kontenerów na jednym
serwerze i prostszy dla administratora klienta. Wybieram Kubernetes, bo celem tego projektu
jest nauka praktyk, które są dziś domyślne w tej roli: deklaratywny stan, kontrolery
uzgadniające rzeczywistość z opisem, rolling update, probe'y, limity zasobów.

**Co tracę:** czytelność dla kogoś, kto nie zna k8s, i istotnie wyższy próg wejścia przy
diagnozie. Sześć kontenerów w `compose.yml` czyta się o drugiej w nocy; dwadzieścia obiektów
w trzech namespace'ach nie.

**Kiedy zmieniam zdanie:** gdy klient nie ma klastra i nie chce go mieć. Wtedy compose jest
uczciwszą odpowiedzią niż zmuszanie go do przyjęcia Kubernetesa razem z produktem.

## 2. Klaster lokalny w k3d zamiast maszyn wirtualnych

**Wybieram tańsze.** k3d uruchamia k3s w kontenerach Dockera i daje prawdziwy klaster
wielowęzłowy: scheduler widzi osobne węzły, więc `kubectl drain`, anti-affinity i PodDisruptionBudget
działają naprawdę, a awarię węzła wywołuję przez `docker stop`. Drugi klaster dla roli „klienta"
powstaje jedną komendą.

**Co tracę:** dwie rzeczy, obie nazwane wprost. Po pierwsze **provisioning** — instalacja k3s na
świeżym systemie, hartowanie hosta, konfiguracja sieci i dysków, upgrade klastra na żywo. Węzły
w k3d wstają gotowe, więc tej warstwy projekt nie uczy. Po drugie **realizm awarii**: wszystkie
węzły dzielą jeden dysk i jedno jądro, więc awaria dysku i partycja sieciowa są nieodwzorowalne.

**Kiedy zmieniam zdanie:** lukę z provisioningu domykam jednym VPS-em za ~5 € przez weekend,
w dowolnym momencie. To jest odłożone, nie porzucone.

## 3. Elasticsearch przez operator ECK, trzy węzły zamiast dwóch

**Wybieram poprawne zamiast intuicyjnego.** Dwa węzły to najgorsza możliwa liczba: przy dwóch
węzłach master-eligible Elasticsearch redukuje voting configuration do jednego, więc utrata
tego konkretnego węzła kładzie klaster. Dwa węzły danych plus jeden mały węzeł voting-only dają
realne kworum. Operator ECK zamiast ręcznych StatefulSetów, bo operatory i CRD to jedna
z rzeczy, po które w ogóle sięga się po Kubernetesa.

**Co tracę:** ~3,3 GB RAM i jeden dodatkowy komponent do zdiagnozowania, gdy zacznie się
zachowywać dziwnie. Przy lokalnym klastrze replika chroni przed awarią węzła — co potrafię
przetestować — ale nie przed awarią dysku, bo dysk jest jeden. Tę drugą własność pokrywa
snapshot, nie replika.

**Kiedy zmieniam zdanie:** gdyby RAM przestał wystarczać, schodzę do jednego węzła i opieram
się wyłącznie na snapshotach. Replika nigdy nie chroniła przed błędem logicznym, więc snapshot
i tak jest konieczny.

## 4. cert-manager: Let's Encrypt przez DNS-01 plus własne CA wewnętrznie

**Wybieram rozwiązanie, które daje obie umiejętności.** Klaster lokalny nie ma wejścia
z internetu, więc wyzwanie HTTP-01 odpada — ale DNS-01 działa odwrotnie: cert-manager wykonuje
połączenie wychodzące do API dostawcy DNS, a Let's Encrypt weryfikuje rekord TXT przez publiczny
DNS, nie dotykając klastra. Publiczne certyfikaty na ruchu wejściowym, własne CA jako drugi
ClusterIssuer dla ruchu między usługami.

**Co tracę:** zależność od zewnętrznego dostawcy DNS i tokenu API, który sam staje się sekretem
do obsłużenia. Koszt: domena ~10 € rocznie.

**Kiedy zmieniam zdanie:** gdy klient nie ma publicznej domeny — wtedy zostaje samo własne CA
i alert o wygasaniu 30/14/7 dni zamiast automatycznego odnawiania.

**Jak wyszło:** oba wydawcy działają na klastrze lokalnym bez wejścia
z internetu. Własne CA: samopodpisany wydawca → certyfikat główny (ECDSA P-256,
pięć lat) → wydawca CA. Let's Encrypt staging i produkcja przez DNS-01
w Cloudflare dla `local.docfind.lol`: certyfikat w 40–80 s, łańcuch produkcyjny
zweryfikowany względem systemowego magazynu zaufania. Wymuszone odnowienie pod
ruchem (`ci/check-tls.sh`): własne CA 59 żądań i staging 126 żądań — zero
nieudanych, proxy podaje nowy certyfikat bez restartu. Na produkcji odnowienie
nie jest wymuszane, bo limit to 5 identycznych certyfikatów na tydzień;
lokalnie domyślny jest staging (`mise.local.toml`).

Koszt, który wyszedł dopiero na żywo: zmiana hosta albo wydawcy na działającym
Gateway daje okno, w którym proxy podaje stary certyfikat dla nowej nazwy — do
wystawienia nowego, przy ACME ponad minutę. Na produkcji zmianę robi się przez
drugi listener z nowym certyfikatem i przełączenie ruchu dopiero po jego
wystawieniu.

## 5. Sekrety w Vault przez External Secrets Operator

**Wybieram bardziej złożone.** Plik z uprawnieniami 600 wystarczyłby przy trzech sekretach.
Wybieram menedżer sekretów, bo pytanie „jak pogodzić GitOps z sekretami" jest jednym z pierwszych,
jakie pada w tej roli, a ESO jest standardową odpowiedzią: w gicie leży `ExternalSecret`,
czyli wskaźnik, nigdy wartość.

**Co tracę:** dwa komponenty więcej, operację unseal Vaulta po każdym restarcie i realne ryzyko,
że sam Vault stanie się przyczyną niedostępności całego systemu.

**Kiedy zmieniam zdanie:** gdyby to była dostawa do klienta bez zespołu utrzymania — wtedy
Sealed Secrets, bo nie ma stanu do odzyskiwania ani operacji unseal.

## 6. API w wielu replikach — odwrócenie wcześniejszej decyzji

**Zmieniłem zdanie i zapisuję dlaczego.** Pierwotnie planowałem jedną instancję API
z uzasadnieniem, że wąskim gardłem jest GPU, więc druga instancja nic nie doda. To było prawdziwe
przy compose, gdzie API i model stały na jednej maszynie. Pod Kubernetesem są to osobne
Deploymenty o niezależnym skalowaniu, więc argument upadł.

Dwie repliki plus PodDisruptionBudget plus `maxSurge: 1, maxUnavailable: 0` dają wdrożenie
i `drain` węzła bez utraty żądania. Przy jednej replice nie byłoby czego uczyć się o HPA,
PDB ani anti-affinity.

**Co tracę:** podwójne zużycie zasobów przez usługę, która i tak głównie czeka na model.

**Kiedy zmieniam zdanie:** gdyby RAM lokalnie okazał się wąskim gardłem — wtedy jedna replika
na co dzień, dwie przy pracy nad wdrożeniami.

## 7. Model jako osobny artefakt, domyślnie poza klastrem

**Wybieram bardziej złożone w pakowaniu.** Model w obrazie to obraz na dziesiątki gigabajtów
i pełny transfer przy każdej zmianie linijki kodu. Model leży w PVC, initContainer pobiera go
z magazynu obiektowego i weryfikuje sumę kontrolną.

Dodatkowo: w klastrze lokalnym Deployment modelu stoi przeskalowany na zero, a domyślnym
backendem jest `stub`. Model na CPU zjadłby 3–4 GB, byłby wolny i nie uczy niczego
o Kubernetesie ponad to, co uczy zwykły Deployment z PVC. Prawdziwy model uruchamiam doraźnie
poza klastrem.

**Co tracę:** prostotę jednego artefaktu, i to, że domyślna ścieżka w klastrze nie jest
ścieżką produkcyjną — więc integracja z prawdziwym modelem jest testowana rzadziej.

**Kiedy zmieniam zdanie:** przy pierwszym błędzie, który `stub` przepuścił, a prawdziwy backend
by wychwycił.

## 8. Monitoring dostarczany z produktem, bez telemetrii do nas

**Wybieram gorsze dla nas i robię to świadomie.** Telemetria dałaby nam wiedzę o problemach
klienta, zanim je zgłosi. Klient ma internet, więc tym razem jest to technicznie wykonalne —
i właśnie dlatego jest to decyzja, a nie ograniczenie. Wysyłanie czegokolwiek ze środowiska
klienta bez jego jawnej zgody jest niedopuszczalne, a w sektorze regulowanym nieakceptowalne
niezależnie od zgody.

**Co tracę:** proaktywność. Zastępuję ją paczką diagnostyczną z historią metryk, więc po
zgłoszeniu widzę, co działo się wcześniej.

**Kiedy zmieniam zdanie:** gdy klient sam poprosi i podpisze na to zgodę — wtedy opt-in,
z jawną listą wysyłanych pól.

## 9. Testy end-to-end w pipeline, na backendzie `stub`

**Wybieram droższe w utrzymaniu.** Pełny test z prawdziwym modelem wymagałby runnera z GPU.
Zamiast rezygnować z e2e, wprowadzam trzeci backend LLM — `stub` — wybierany tym samym
przełącznikiem konfiguracyjnym co `ollama` i `vllm`, dający deterministyczne wyjście. Pipeline
stawia efemeryczny klaster k3d, instaluje chart i sprawdza całą ścieżkę zapytanie → fragmenty
→ odpowiedź na wersjonowanym korpusie z `tests/corpus/`.

**Co tracę:** e2e nie pokrywa prawdziwej integracji z modelem, więc regresja po stronie
promptu albo formatu odpowiedzi backendu przejdzie przez pipeline. Dochodzi też trzecia
implementacja backendu do utrzymania.

**Kiedy zmieniam zdanie:** gdy pojawi się dostęp do runnera z GPU — wtedy `stub` zostaje dla
szybkiej ścieżki na PR, a nocny bieg idzie na prawdziwym modelu.

## 10. GitOps na ArgoCD, wdrożony wcześnie

**Wybieram ArgoCD, nie Flux** — interfejs realnie pomaga przy nauce, a narzędzie jest częstsze
w ogłoszeniach. Ważniejsza jest kolejność: ArgoCD wchodzi na Etapie 5, przed Elasticsearchem
i przed LLM. Budowanie pięciu usług przez `kubectl apply`, a potem konwersja na GitOps, to
przepisywanie tego samego dwa razy.

**Co tracę:** ~0,7 GB RAM i warstwę pośrednią, która sama potrafi być przyczyną awarii —
zablokowana synchronizacja wygląda dokładnie jak zepsuta aplikacja.

**Kiedy zmieniam zdanie:** przy jednym środowisku i jednej osobie GitOps jest kosztem bez
zysku. Tu zysk jest dydaktyczny i to wystarczający powód.

## 11. Gateway API z Envoy Gateway — zmiana decyzji, wcześniej ingress-nginx

**Zmieniłem zdanie, bo spełnił się warunek zmiany — mocniej, niż zakładałem.**
Pierwotnie wybrałem ingress-nginx jako „najpowszechniejszy”, z warunkiem zmiany
„gdy Gateway API stanie się domyślne”. W listopadzie 2025 SIG Network ogłosił
wycofanie ingress-nginx: od marca 2026 bez wydań, bez poprawek błędów i bez łatek
bezpieczeństwa; oficjalna rekomendacja to migracja na Gateway API. Wdrażanie
w październiku 2026 kontrolera, który od siedmiu miesięcy nie dostaje łatek,
na ścieżce całego ruchu wejściowego, nie da się obronić.

Envoy Gateway zamiast Traefika i NGINX Gateway Fabric: projekt CNCF na Envoyu,
czyli tym samym proxy co Istio i Contour, więc wiedza przenosi się na service
mesh. Identyfikator żądania, timeouty i strumieniowanie ma natywnie.

Podział odpowiedzialności: Gateway, wydawcy certyfikatów i przekierowanie
HTTP→HTTPS należą do platformy (`deploy/charts/platform`), HTTPRoute do
aplikacji (`deploy/charts/docfind`). Listener HTTPS przyjmuje trasy tylko
z przestrzeni nazw z etykietą nadaną przez platformę — domyślne „All”
pozwoliłoby dowolnej przestrzeni nazw przejąć ruch dla dowolnej nazwy hosta.

**Co tracę:** prostotę. Envoy Gateway to kontroler plus osobne pody proxy, które
kontroler tworzy w trakcie działania — ich repliki, zasoby i PDB ustawia się
w zasobie EnvoyProxy, a czego tam nie ma (zasoby shutdown-managera), przez patch
na wygenerowanym Deploymencie. Tych podów nie widać w `helm template`, więc
polityki sprawdzam także na żywym klastrze (`ci/check-tls.sh`). Część
ustawień — limit bezczynności połączeń do backendu — to CRD specyficzne dla
Envoy Gateway; u klienta z inną implementacją Gateway API wyłącza się je
wartością `route.envoyGatewayPolicies`.

**Kiedy zmieniam zdanie:** gdy klient ma już własny Gateway — wtedy chart
aplikacji podpina HTTPRoute pod jego Gateway, a chart platformy nie jest
instalowany.

## 12. Storage local-path, bez Longhorna

**Wybieram prostsze i to jest moja decyzja „świadomie mniej".** Longhorn dałby replikację
bloków między węzłami i nauczyłby storage rozproszonego. Elasticsearch replikuje już na poziomie
aplikacji — replikowanie także pod spodem to dwa razy ten sam koszt za tę samą własność,
przy podwojonym zużyciu dysku i drugim systemie do diagnozy.

**Co tracę:** PV są przypięte do węzłów, więc pod ze stanem nie przeniesie się na inny węzeł.
Przy awarii węzła ratuje mnie replika ES, nie storage.

**Kiedy zmieniam zdanie:** przy pierwszym komponencie stanowym, który **nie** replikuje sam —
na przykład bazie relacyjnej pod metadane.

## 13. Dostawa jako chart Helma w formacie OCI

**Wybieram artefakt, nie skrypt.** Chart wersjonowany razem z aplikacją, publikowany do tego
samego rejestru co obrazy. Instalacja to `helm install`, aktualizacja `helm upgrade`, a cofnięcie
`helm rollback` — deklaratywnie i z historią wydań po stronie klienta.

**Co tracę:** klient musi mieć Helma i dostęp do naszego rejestru, a sam chart staje się
publicznym interfejsem produktu: każda zmiana nazwy wartości jest zmianą łamiącą zgodność.

**Kiedy zmieniam zdanie:** gdyby klient wymagał instalacji bez połączenia z rejestrem — wtedy
wraca lustro rejestru i tarball airgapowy, czyli warstwa, z której tu zrezygnowałem.

## 14. Obraz na Alpine zamiast Debian slim

**Wybieram mniejsze kosztem wolniejszego zamykania.** Zmierzone na tym samym
kodzie: Debian slim daje obraz 228 MB i koszt własny zamykania 0,373 s, Alpine
daje 120 MB i 0,474 s. Limit 200 MB z Etapu 1 spełnia tylko Alpine — baza
`python:3.12-slim` to ~196 MB przy venv ważącym 54 MB, więc na niej nie da się
zejść niżej bez porzucenia Pythona.

**Co tracę:** musl zamiast glibc. Dwie konkretne konsekwencje. Zamykanie procesu
jest o ~0,1 s wolniejsze, co przy rolling update mnoży się przez liczbę replik.
Groźniejsze jest to, że resolver musl historycznie inaczej obsługuje domeny
wyszukiwania z `/etc/resolv.conf` niż glibc — a w Kubernetesie to jest dokładnie
mechanizm, którym pody odnajdują usługi po nazwie.

**Kiedy zmieniam zdanie:** przy pierwszym problemie z rozwiązywaniem nazw
w klastrze, którego nie da się wyjaśnić inaczej. Wracam wtedy na Debiana
i przyjmuję 228 MB — po zniknięciu paczki offline rozmiar obrazu kosztuje
transfer z rejestru, a nie miejsce w archiwum instalacyjnym, więc jest to
koszt, który da się znieść.

## 15. Obraz bez instrukcji HEALTHCHECK

**Wybieram pominięcie mechanizmu, który w docelowym środowisku nie istnieje.**
Kubernetes ignoruje `HEALTHCHECK` z obrazu i korzysta wyłącznie
z `livenessProbe` i `readinessProbe` z manifestu (Etap 2). Zmierzony koszt
utrzymywania go mimo to: 0,24 s dłuższe zamykanie kontenera oraz start całego
interpretera Pythona co 10 sekund, co pod limitem CPU w klastrze nie jest
zerowe.

**Co tracę:** `docker ps` przestaje pokazywać stan zdrowia przy lokalnym
uruchomieniu, więc pracując poza klastrem trzeba odpytać `/healthz` samemu.

**Kiedy zmieniam zdanie:** gdyby produkt miał być kiedykolwiek uruchamiany
przez docker compose — wtedy healthcheck wraca, bo compose go czyta i używa
do kolejności startu.

## 16. Limit pamięci tak, limit CPU nie

**Wybieram asymetrię.** Pamięć jest zasobem nieściśliwym: pod, który jej
przekroczy, zostaje zabity przez OOM killera, a bez limitu jeden wyciek potrafi
wypchnąć z węzła sąsiednie pody. Limit pamięci chroni więc węzeł. CPU jest
zasobem ściśliwym: bez limitu pod po prostu korzysta z wolnych cykli, a przy
limicie jest dławiony przez CFS nawet wtedy, gdy węzeł stoi bezczynny —
co wygląda jak nagły wzrost opóźnień bez żadnej przyczyny widocznej w aplikacji.
Request CPU zostaje, bo na nim opiera się scheduler i przydział cykli przy
konkurencji.

**Co tracę:** pod bez limitu CPU może przy błędzie zająć wszystkie wolne rdzenie
węzła. Przed zagłodzeniem sąsiadów chronią ich requesty, ale nie przed spadkiem
wydajności poniżej tego, do czego się przyzwyczaili.

**Kiedy zmieniam zdanie:** w klastrze współdzielonym z cudzymi obciążeniami albo
tam, gdzie administrator klienta wymusza limity przez LimitRange lub politykę —
wtedy limit CPU, ale z zapasem kilkukrotnie ponad request.

## 17. Klaster lokalny na portach nieuprzywilejowanych, tylko na loopbacku

**Wybieram mniej wygodne adresy zamiast rozluźnienia zabezpieczeń hosta.**
Docker działa tu bez roota, więc nie otworzy portów 80 i 443. Dokumentacja
Dockera proponuje obniżyć `net.ipv4.ip_unprivileged_port_start`, ale to odblokowuje
cały zakres do 1023 dla każdego procesu w systemie, a nie tylko porty klastra.
Load balancer słucha więc na 8080 i 8443, a API Kubernetesa na 6550. Wszystko
jest związane z `127.0.0.1`, bo k3d domyślnie wiąże porty z `0.0.0.0` — w sieci
biurowej albo przy WSL w trybie mirrored API klastra z uprawnieniami admina
byłoby osiągalne z zewnątrz.

**Co tracę:** adresy z portem (`https://docfind.<domena>:8443`) i to, że lokalny
adres różni się od produkcyjnego. Wyzwania ACME to nie dotyka, bo DNS-01
(decyzja 4) nie potrzebuje portu 80.

**Kiedy zmieniam zdanie:** nigdy dla portów — rozwiązanie działa bez sudo, więc
też na firmowym laptopie bez uprawnień administratora. Wiązanie z loopbackiem
zmieniam tylko wtedy, gdy klaster ma być świadomie dostępny dla innej maszyny,
i wtedy z konkretnym adresem interfejsu, nigdy z `0.0.0.0`.

## 18. Własny CoreDNS z charta zamiast wbudowanego w k3s

**Wybieram przejęcie komponentu, który dystrybucja dostarcza gotowy.** Pierwszy
drain przeszedł z 4 nieudanymi żądaniami na 153. Diagnoza z czasów faz curl
wykazała, że wszystkie zawiodły na rozwiązywaniu nazwy (`exit=28`, `dns=0`), a nie
na API — razem z repliką API wyjechał z węzła jedyny pod CoreDNS. k3s uruchamia
go w jednej replice, bez PodDisruptionBudget, a ręczne skalowanie nie przetrwa
restartu serwera, bo k3s ponownie aplikuje własne manifesty. CoreDNS jest
teraz instalowany z charta `coredns/coredns` o przypiętej wersji i sumie: dwie
repliki, PDB, rozłożenie na węzły, `system-cluster-critical`, bez limitu CPU.
Po zmianie: trzy draine, każdego węzła z repliką API, 1358 żądań, zero błędów —
a przy ostatnim PDB wstrzymał drain, dopóki druga replika DNS nie była gotowa.

**Co tracę:** aktualizacje CoreDNS są teraz moje, a nie przychodzą z k3s, więc
wersja może zostać w tyle za dystrybucją. Znika też `host.k3d.internal`, który
k3d wstrzykuje tylko do wbudowanego CoreDNS. `metrics-server`
i `local-path-provisioner` zostają wbudowane, z jedną repliką — świadomie, bo
nie leżą na ścieżce żądania: ich chwilowy brak wstrzymuje `kubectl top` i nowe
wolumeny, nie ruch.

**Kiedy zmieniam zdanie:** gdy k3s pozwoli ustawić liczbę replik i PDB dla
CoreDNS w swojej konfiguracji — wtedy wracam do wbudowanego i jednej rzeczy
mniej do aktualizowania.

## 19. Docker rootless zostaje, mimo kosztów

**Wybieram trudniejsze, bo bezpieczniejsze.** Klaster da się postawić na
zwykłym Dockerze bez żadnego z poniższych obejść. Rootless oznacza, że
ucieczka z kontenera daje uprawnienia użytkownika, a nie roota hosta — a to
jest właściwość, której środowisko deweloperskie nie powinno oddawać za wygodę.

**Co tracę:** cztery rzeczy, każda z własnym obejściem. Kontroler `cpuset`
trzeba delegować do sesji użytkownika (jednorazowo, sudo). Porty poniżej 1024
są niedostępne — stąd 8080/8443 (decyzja 17). `k3d image import` w trybie
domyślnym zawodzi i działa tylko `--mode direct`. Kubelet wymaga bramki
`KubeletInUserNamespace`, która w Kubernetesie 1.36 jest wciąż **alfa** —
czyli lokalny klaster opiera się na funkcji bez gwarancji stabilności.
Dokładam ją tylko przy rootless, żeby nie łagodzić kubeletowi obsługi błędów
tam, gdzie nie trzeba.

**Kiedy zmieniam zdanie:** gdy bramka `KubeletInUserNamespace` zniknie albo
zmieni zachowanie w kolejnej wersji Kubernetesa — wtedy klaster lokalny
przechodzi na Dockera z rootem, a rootless zostaje dla pozostałej pracy.

## 20. Wszystko z zewnątrz przypięte digestem albo SHA commita

**Wybieram niezmienne referencje zamiast czytelnych.** Tag obrazu i tag akcji
GitHuba są przesuwalne: właściciel może pod tą samą nazwą opublikować inną
zawartość, a pipeline pobierze ją bez żadnego sygnału. Tak w 2025 roku
skompromitowano `tj-actions/changed-files` — przesunięte tagi, złośliwy commit,
sekrety każdego pipeline'u z `@v…`. Binarki i charty były już weryfikowane
sumą; obrazy i akcje nie, co było niespójne. Teraz: obrazy narzędzi, k3s, bazy
w Dockerfile i obraz CoreDNS mają digest indeksu (działa na każdej
architekturze), akcje pełne SHA z komentarzem wersji. Polityka w
`ci/check_policy.py` odrzuca obraz komponentu platformy bez digestu.

**Luki znalezione po wprowadzeniu.** Przypięcie objęło to, co projekt pobiera
sam, ale nie to, co w trakcie biegu pobierały za niego narzędzia:
`setup-uv` bez wersji instalował najnowszego uv, `setup-buildx-action` brał
przesuwalny tag BuildKitu, `ubuntu-latest` zmienia system i jego `python3`,
kubeconform przy każdym biegu pobierał schematy z gałęzi `master`, a `uv sync`
instalował projekt w trybie edytowalnym i przy tym najnowszego hatchlinga,
którego nie ma w `uv.lock`. Teraz uv w CI ma wersję z Dockerfile, BuildKit
digest z `ci/lib.sh`, runner to `ubuntu-24.04`, schematy pochodzą z
przypiętego commita, a projekt nie jest budowany (`tool.uv.package = false`).
Dowodem był bieg kubeconform bez sieci — z domyślnymi ustawieniami padał.
Przypięcie sprawdza się pytaniem „co jeszcze to pobiera, kiedy biegnie", a nie
„co wpisałem do pliku".

**Co tracę:** czytelność — `@sha256:edad48e1…` nic nie mówi bez komentarza —
i darmowe łatki bezpieczeństwa, które przy tagu przychodziły same. Tę drugą
stratę pokrywa Dependabot (akcje, bazy w Dockerfile, uv.lock), ale **nie**
przypięcia w `ci/lib.sh` ani narzędzia w `mise.toml` (decyzja 23): k3s,
BuildKit, charty, schematy i binarki aktualizuję ręcznie, razem z sumami.

**Kiedy zmieniam zdanie:** nigdy dla samego przypinania. Ręczne aktualizacje
z `ci/lib.sh` i `mise.toml` przeniosę do Renovate, gdy zaczną zalegać.

## 21. Build powtarzalny bajt w bajt

**Wybieram dowód zamiast założenia.** Ten sam commit budowany dwa razy dawał
różne obrazy, więc każdy build wywoływał rollout, a digestu z klastra nie
dało się powiązać z zawartością. Teraz: czas w obrazie to czas commita
(`SOURCE_DATE_EPOCH`), czasy plików w warstwach są do niego przycinane,
użytkownik nie powstaje przez `adduser` (zapisywał dzisiejszą datę w
`/etc/shadow`), a kod aplikacji jest kopiowany jako pliki z bytecode'em
`checked-hash`, zamiast instalowany przez uv — który zapisywał w dist-info
ctime źródła, niemożliwe do ustawienia z przestrzeni użytkownika. Każdą z tych
przyczyn znalazło porównanie warstwa po warstwie (`ci/compare_oci.py`).

Czasy modyfikacji są **ustawiane** na czas commita, a nie tylko przycinane.
`rewrite-timestamp` w BuildKit przycina wyłącznie czasy nowsze od
`SOURCE_DATE_EPOCH`, a starsze zostawia — te pochodzą z kontekstu budowania
i z warstw w pamięci podręcznej, czyli od stanu buildera. Pierwsza wersja
tej decyzji twierdziła, że dwa sterowniki BuildKit dają ten sam obraz; test,
na którym to oparłem, brał warstwy z pamięci podręcznej. Po poprawce:
identyczny digest przy buildzie z pamięcią podręczną, od zera, na sterowniku
lokalnym i na tym z CI. `ci/check-reproducible.sh` porównuje build z pamięcią
podręczną z buildem od zera w każdym pipeline'ie.

Zgodność z CI zależy od wersji BuildKit, bo frontend Dockerfile jest w nią
wbudowany — a `setup-buildx-action` brał przesuwalny tag. CI używa teraz
BuildKitu przypiętego digestem (`DF_BUILDKIT_IMAGE` w `ci/lib.sh`), w tej samej
wersji co lokalny Docker; `check-reproducible.sh` ostrzega, gdy lokalna wersja
się rozjedzie.

**Co tracę:** trzy rzeczy. Pole `built_at` w `/version` zmieniło nazwę na
`source_date`, bo podaje czas commita, a nie budowania — zmiana kontraktu.
Lokalne obrazy nie mają atestacji pochodzenia, bo ta z natury zmienia digest
co build; publikowane do rejestru mają. Pakiet `docfind_api` nie jest
zainstalowany, więc nie ma go w `importlib.metadata` — kod nie korzysta
z tego, ale narzędzie do inwentaryzacji zależności go nie zobaczy.

**Kiedy zmieniam zdanie:** gdy uv przestanie zapisywać `uv_cache.json`
z ctime albo pozwoli to wyłączyć — wtedy wracam do instalacji koła, bo to
zwyklejszy układ dla każdego, kto otworzy obraz.

## 22. Łatki od Dependabota scalane automatycznie, za bramką CI

**Wybieram automat z warunkami, nie automat bez warunków.** Przypięcia z decyzji
20 bez aktualizacji się starzeją, a ręczne scalanie każdej łatki to praca, która
w końcu przestaje być robiona. Łatki (`version-update:semver-patch`) i
odświeżenia digestu obrazu bazowego są więc scalane automatycznie przez
`dependabot/fetch-metadata` i `gh pr merge --auto`, a minor i major czekają na
przegląd. Warunki, bez których to byłoby niebezpieczne albo nie działało:

- **Bramka.** `--auto` czeka tylko na sprawdzenia *wymagane* przez regułę
  gałęzi. Reguła `main` (`.github/rulesets/main.json`, stosowana przez
  `ci/apply-repo-settings.sh`) wymaga zadań `test` i `build`, przypiętych do
  aplikacji GitHub Actions, żeby status nie mógł zgłosić ktoś inny.
- **Build na PR-ach.** Wcześniej zadanie `build` biegło tylko po scaleniu — PR #7
  od Dependabota zmienił obraz builda i wszedł do `main`, zanim ktokolwiek
  zbudował z nim obraz. Teraz każdy PR buduje obraz, sprawdza bazę, testy na
  musl, warunki Etapu 1 i powtarzalność; publikacja tylko przy push.
- **Scalanie tokenem aplikacji.** Zdarzenia wywołane przez `GITHUB_TOKEN` nie
  uruchamiają workflowów, więc PR scalony w jego imieniu wchodził do `main` bez
  biegu `ci.yml` na push — bez builda i bez publikacji obrazu. Auto-merge włącza
  teraz token aplikacji GitHuba (sekrety Dependabota
  `DOCFIND_AUTOMERGE_CLIENT_ID` i `DOCFIND_AUTOMERGE_PRIVATE_KEY`), ograniczony
  do tego repozytorium i do dwóch uprawnień. Bez skonfigurowanej aplikacji
  workflow niczego nie scala i zostawia ostrzeżenie — PR czeka na człowieka,
  zamiast wejść bez obrazu.
- **Typ aktualizacji, który naprawdę przychodzi.** `fetch-metadata` 2.5.0
  zwracał pusty `update-type` dla PR-ów Pythona, więc łatki z `uv.lock` nigdy
  nie scalały się same (poprawione w 3.1.0). Odświeżenie digestu bez zmiany tagu
  (`python:3.14-alpine@sha256:A` → `B`) fetch-metadata klasyfikuje błędnie jako
  major (dependabot/fetch-metadata#726), więc takie PR-y — większość łatek
  bezpieczeństwa bazy — też czekały na człowieka. Workflow rozpoznaje je teraz
  po poprzedniej wersji w postaci digestu i traktuje jak łatkę.
- **Nowe wydanie Alpine to nie łatka.** Pod tym samym tagiem `3.14-alpine`
  pojawia się też nowe wydanie Alpine — nowy musl i OpenSSL. `ci/check-image-base.sh`
  porównuje zbudowany obraz z `DF_BASE_ALPINE` w `ci/lib.sh` i zatrzymuje build,
  dopóki człowiek nie podbije tej wartości w tym samym PR-ze.
- **Cooldown.** Nowa wersja jest proponowana dopiero 7 dni po wydaniu, major po
  14. Pierwotnie łatki czekały 3 dni; zizmor w `run-tests.sh` wymaga co
  najmniej 7, a koszt jest mały — aktualizacje bezpieczeństwa z alertów
  Dependabota cooldownem nie są objęte. Skompromitowane wydania bywają
  wycofywane w ciągu godzin albo dni — automat scalający świeże wydanie ufałby
  mu, zanim ktokolwiek je obejrzał.

Workflow używa `pull_request`, nie `pull_request_target`, `GITHUB_TOKEN` ma
w nim tylko odczyt, a metadane PR-a idą przez zmienne środowiskowe zamiast do
skryptu. Workflowy sprawdzają actionlint i zizmor w `run-tests.sh`.

**Co tracę:** łatka, której testy nie pokrywają, wejdzie bez ludzkiego oka —
bramka jest tak dobra jak CI, a CI nie ćwiczy jeszcze aplikacji na klastrze
(e2e przychodzi na Etapie 9). Aplikacja GitHuba to nowy sekret z prawem zapisu:
klucz prywatny trzeba chronić i rotować. Reguła blokuje też bezpośredni push
i force-push do `main`, więc przepisanie historii, jak przy poprawce tożsamości
commitów, wymagałoby jej tymczasowego wyłączenia. `strict` w regule jest
wyłączone, więc dwie łatki zielone osobno mogą wejść po sobie bez testu
kombinacji; kolejka scalania (merge queue), która to rozwiązuje, nie jest
dostępna dla repozytoriów na koncie osobistym.

**Kiedy zmieniam zdanie:** przy pierwszej automatycznie scalonej łatce, która
zepsuła `main` — wtedy auto-merge tylko dla zależności deweloperskich, dopóki
e2e z Etapu 9 nie domknie luki. Przy przejściu na Renovate (decyzja 20) ten
workflow znika: Renovate scala sam, własnym tokenem aplikacji, i rozróżnia
odświeżenie digestu od zmiany wersji.

## 23. mise jako jedno wejście do narzędzi i zadań

**Wybieram jedno narzędzie zamiast skryptu instalacyjnego i luźnych poleceń.**
Wcześniej binarki instalował `ci/install-tools.sh` — tylko dla linux/amd64, do
globalnego `~/.local/bin` wspólnego dla wszystkich projektów — a shellcheck,
actionlint, helm i kubeconform biegły w przypiętych kontenerach. Teraz
`mise.toml` przypina wersje, `mise.lock` trzyma URL i sumę dla każdej platformy
(także macOS i arm64), a `locked = true` odmawia instalacji czegokolwiek spoza
lockfile'a. Sumy kubectl, k3d, helm i kubeconform w `mise.lock` są bajt w bajt
tymi, które były w `ci/lib.sh` — te same artefakty z tych samych adresów.
Samo mise wchodzi do repozytorium jako `bin/mise`: skrypt z przypiętą wersją
i sumami, które przy przypięciu zgadzały się z plikiem sum podpisanym kluczem
GPG autorów (odcisk z dokumentacji mise). Wszystko ląduje w `.mise/` w
repozytorium — bez globalnej instalacji i bez aktywacji w powłoce, tak samo
lokalnie i w CI. Zadania w `mise.toml` tylko wołają skrypty z `ci/`, więc
skrypty działają też bez mise. `run-tests.sh` nie potrzebuje już Dockera i jest
szybszy (5,6 s zamiast 8,6 s lokalnie).

**Co tracę:** Dependabot nie obsługuje mise — narzędzia z `mise.toml` aktualizuję
ręcznie (`./bin/mise lock`), tak jak wcześniej przypięcia z `ci/lib.sh`. Jest
jedno narzędzie więcej do zrozumienia, z własnymi pułapkami: zadania TOML biegną
w `sh` bez `pipefail`, a zależności zadań (`depends`) równolegle. uv zostaje
poza mise: jego wersja jest w Dockerfile, bo tam aktualizuje ją Dependabot —
druga kopia w `mise.toml` rozjeżdżałaby się przy każdej jego łatce.

**Kiedy zmieniam zdanie:** gdy ręczne aktualizacje `mise.toml` i `ci/lib.sh`
zaczną zalegać — wtedy Renovate, który obsługuje mise, Dockerfile, akcje i uv
naraz; uv przejdzie wtedy do `mise.toml`, bo jedna grupa aktualizacji podbije
obie kopie jednocześnie.

## 24. Testy jednostkowe także w obrazie, na musl

**Wybieram dłuższy pipeline zamiast założenia, że Python to Python.** Testy
biegły na interpreterze hosta: lokalnie systemowy Python 3.12.3 z glibc, w CI
`python3` runnera, a `requires-python = ">=3.12"` przyjąłby tam każdą nowszą
wersję. Obraz biegnie na Pythonie 3.12.14 i musl, z innymi binariami
pydantic-core, uvloop i httptools (koła musllinux zamiast manylinux). Teraz etap
`test` w Dockerfile, zbudowany na bazie buildera, uruchamia pytest w obrazie
(`ci/test-image.sh`, zadanie build w CI). `.python-version` przypina wersję
testów na hoście, a `run-tests.sh` i `check-image-base.sh` pilnują, żeby Python
testów, tag bazy i zbudowany obraz miały tę samą wersję.

**Co tracę:** ~20 s pierwszego biegu na każdym PR-ze i testy w kontekście
builda — `.dockerignore` przepuszcza `tests/`, choć runtime ich nie kopiuje.
Etap `test` potrzebuje też `deploy/config/app.yml.example` spoza kontekstu
usługi, więc bez `ci/test-image.sh` (nazwany kontekst `config`) się nie zbuduje.

**Kiedy zmieniam zdanie:** gdy baza zejdzie z musl (odwrócona decyzja 14) —
wtedy testy na hoście z przypiętą wersją Pythona dają prawie to samo
mniejszym kosztem.

## 25. Diagnostyka asynchroniczna, wyszukiwanie synchroniczne

**Wybieram dwa modele wykonania w jednej aplikacji.** Handler `def` Starlette
wykonuje w puli wątków anyio, wspólnej dla procesu i ograniczonej do 40 wątków.
Gdy zajmą ją wolne wywołania do Elasticsearcha albo modelu, synchroniczne
`/healthz` czeka na wolny wątek, przekracza timeout sondy i kubelet restartuje
zdrowy pod pod obciążeniem — wolna zależność zamienia się w restarty API.
Endpointy diagnostyczne są więc `async def` i nie blokują; test z pulą
ograniczoną do jednego zajętego wątku pada, gdy `/healthz` wraca do `def`.
`/search` zostaje `def`, dopóki klient ES i modelu nie jest asynchroniczny —
blokujące wywołanie w `async def` zatrzymałoby pętlę zdarzeń razem z sondami.
Przy okazji `/docs` jest domyślnie wyłączone: Swagger UI ładuje JS i CSS
z cdn.jsdelivr.net, czyli w przeglądarce użytkownika klienta wykonywałby się
kod z obcego serwera.

**Co tracę:** regułę, której nie widać w typach — w `async def` nie wolno
blokować; ruff `ASYNC` łapie część przypadków, ale nie synchronicznego klienta
biblioteki. `def` w `Depends` też trafia do puli wątków, więc zależności
diagnostyki muszą być `async def`. Swagger UI trzeba włączyć w konfiguracji
(`service.docs: true`).

**Kiedy zmieniam zdanie:** przy asynchronicznym kliencie ES i modelu
(Etapy 6–7) — wtedy `/search` przechodzi na `async def` i podział znika.

## 26. Kubeconfig projektu zamiast globalnego

**Wybieram mniej wygodne, bo bezpieczniejsze.** k3d dopisywał klaster do
`~/.kube/config` i przełączał bieżący kontekst — każdy `kubectl` w dowolnym
terminalu wskazywał nagle na lokalny klaster, a skrypty dziedziczyły kontekst
z powłoki. `check-drain.sh` sprawdzał kontekst przed drainem, reszta skryptów
nie. Teraz dane dostępowe trafiają do `.cache/kubeconfig`, który ustawia
`ci/lib.sh` dla każdego skryptu i `mise.toml` dla `./bin/mise exec`, a
`deploy-local.sh` odtwarza ten plik z k3d przy każdym uruchomieniu.

**Co tracę:** zwykłe `kubectl` w terminalu nie widzi lokalnego klastra —
trzeba `./bin/mise exec -- kubectl …` albo `export KUBECONFIG=$PWD/.cache/kubeconfig`.

**Kiedy zmieniam zdanie:** nigdy dla skryptów. Dla pracy interaktywnej wygoda
wraca bez zmiany zasady, gdy mise jest aktywowane w powłoce — wtedy ustawia
`KUBECONFIG` samo po wejściu do katalogu projektu.

## 27. Python 3.14, choć system ma 3.12

**Wybieram nowszy interpreter niż ten, który daje dystrybucja.** Dependabot
zaproponował bazę `python:3.14-alpine`. 3.12 dostaje już tylko poprawki
bezpieczeństwa (koniec wsparcia 10.2028), 3.14 — poprawki błędów do ok. 10.2027
i bezpieczeństwa do 10.2030, a upgrade jest najtańszy teraz, zanim dojdą klienci
Elasticsearcha i modelu. Ubuntu 24.04 ma systemowy Python 3.12.3 i tak zostaje:
należy do systemu. Testy biegną na Pythonie 3.14 pobranym przez uv do
`~/.local/share/uv/python/` (adres i suma SHA-256 są wbudowane w binarkę uv,
więc wersję interpretera wyznacza przypięta wersja uv). Host i obraz mają teraz
tę samą łatkę — 3.14.7 — a wcześniej systemowe 3.12.3 różniło się od 3.12.14
w obrazie o jedenaście wydań. W CI jest tak samo, bo runner `ubuntu-24.04` też
ma 3.12.

**Co tracę:** trzy rzeczy. Kod może używać składni tylko dla 3.14 (ruff już zdjął
nawiasy w `except OSError, json.JSONDecodeError:`), więc uruchomiony systemowym
`python3` kończy się `SyntaxError` — testy tylko przez `uv run` albo
`./bin/mise run test`, a edytor musi wskazywać `.venv` usługi. Skrypty w `ci/`,
które biegną na systemowym `python3` (`compare_oci.py`, wstawki w skryptach
bashowych), zostają na 3.12 — pilnują tego `ci/ruff.toml` i mypy z
`--python-version 3.12`. Pierwsza synchronizacja na nowej maszynie potrzebuje
sieci, bo pobiera interpreter.

**Kiedy zmieniam zdanie:** gdy zależność nie ma kół musllinux dla bieżącej wersji
Pythona (build w `test-image.sh` przerwie się, bo w obrazie nie ma kompilatora),
albo gdy środowisko pracy zabroni interpreterów pobieranych przez uv — wtedy
wersja z dystrybucji i ta sama w obrazie. Kolejne wersje minor (3.15.0 wychodzi
1.10.2026) przyjmuję po jednym–dwóch wydaniach poprawkowych: czerwony PR od
Dependabota zamykam komentarzem `@dependabot ignore this minor version`.

---

## 28. Schematy CRD generowane z przypiętych chartów

**Wybieram walidację względem tego, co faktycznie instaluję.** kubeconform
w trybie `-strict` potrzebuje schematu dla każdego zasobu, także z CRD
(Gateway, HTTPRoute, Certificate, EnvoyProxy). Gotowe katalogi schematów CRD
nie nadążają za wydaniami, więc walidacja względem starszej wersji
przepuszczałaby pola, których API server nie przyjmie, albo odrzucała nowe.
`ci/crd_schemas.py` generuje schematy z CRD wyrenderowanych z przypiętych
chartów, z `additionalProperties: false` wszędzie, gdzie CRD nie dopuszcza
nieznanych pól — literówka w polu zasobu jest błędem, tak jak dla zasobów
wbudowanych. Sprawdzone w obie strony: literówka `requestId` i zła wartość enuma
są odrzucane.

**Co tracę:** dwie rzeczy. Sam rodzaj CustomResourceDefinition nie ma schematu
w repozytorium yannh, więc jest pominięty jawnie (`-skip`), nie przez
`-ignore-missing-schemas`, które wyłączyłoby walidację wszystkiego bez schematu.
Pola oznaczone w CRD jako dowolne (patch w EnvoyProxy) nie są sprawdzane
niczym — tam rozstrzyga dopiero polityka na żywym podzie.

**Kiedy zmieniam zdanie:** gdy kubeconform zacznie czytać CRD wprost — wtedy
generator jest zbędny.

## 29. Identyfikator żądania nadawany na wejściu, nie przyjmowany od klienta

**Wybieram identyfikator, któremu mogę ufać.** Envoy nadaje `X-Request-Id`
każdemu żądaniu (`requestID: Generate`), zapisuje go w logu dostępowym w JSON
i przekazuje do API, które odsyła go w odpowiedzi. Jeden identyfikator łączy
zgłoszenie klienta, wiersz logu proxy i wiersz logu aplikacji. Nie
`PreserveOrGenerate`: identyfikator od klienta z internetu pozwoliłby sklejać
niezwiązane wpisy albo zalać logi cudzym identyfikatorem. API niczego samo nie
generuje — identyfikator, którego nie ma w logu proxy, niczego nie łączy —
i odrzuca wartości spoza bezpiecznego formatu, bo bywa wołane w klastrze
z pominięciem proxy.

**Co tracę:** klient nie może przekazać własnego identyfikatora do korelacji
ze swoimi logami — musi wziąć nasz z odpowiedzi.

**Kiedy zmieniam zdanie:** gdy przed naszym Gateway stanie zaufany load
balancer klienta, który sam nadaje identyfikator — wtedy `Preserve` na
połączeniach tylko od niego.

## Czego bym dziś nie powtórzył

Najważniejsza część tego dokumentu i najrzadziej przygotowana — sekcja pusta
na koniec projektu oznacza, że projekt niczego nie nauczył.

**Kolejność instalacji wyprowadzona z wygody, nie z zależności.** Instalowałem
cert-manager przed Envoy Gateway, bo „certyfikaty są potrzebne Gateway”.
Zależność szła w drugą stronę: cert-manager z obsługą Gateway API wymaga przy
starcie CRD Gateway API, które instaluje chart Envoy Gateway — i bez nich nie
czeka, tylko wpada w CrashLoopBackOff. Dziś kolejność instalacji wypisuję
z tego, czego każdy komponent potrzebuje przy starcie.

**Limit, którego nie było widać, bo nikt go nie dzielił.** Envoy padał z SIGSEGV
przy rolling update proxy. Przyczyna: przy Dockerze rootless wszystkie procesy
we wszystkich kontenerach dzielą limit instancji inotify jednego użytkownika
hosta (128), a rolling update na chwilę podwaja liczbę podów proxy. Test
odnowienia certyfikatu zobaczył to jako połowę nieudanych żądań i łatwo byłoby
szukać winy w podmianie certyfikatu. Dziś przy objawie „pod się restartuje”
pierwsze jest `logs --previous`, a nie hipoteza o tym, co właśnie testuję.

**Szukanie przyczyny, zanim sprawdzę, czy proces w ogóle żyje.** Klaster nie
wstawał, agenci widzieli `connection reset` do serwera. Zbadałem DNS
w węzłach, reguły NAT i `route_localnet` — i dopiero wtedy zauważyłem, że
procesu k3s nie ma, a kontener stoi, bo trzyma go skrypt startowy k3d.
Przyczyna była w logu serwera od początku (`Failed to start ContainerManager`).
Dziś kolejność jest odwrotna: najpierw czy proces istnieje i na czym słucha,
potem sieć.

**`cmd | grep -q` przy `set -o pipefail`.** `docker buildx inspect | awk
'… exit'` przerywał build bez żadnego komunikatu, ale nie zawsze — w pomiarze
8 razy na 30. Konsument kończy się po pierwszym dopasowaniu, producent dostaje
SIGPIPE, `pipefail` uznaje potok za nieudany, `set -e` kończy skrypt. W `if`
działa to odwrotnie i daje fałszywe „nie”. Ten sam wzorzec siedział w sześciu
miejscach w `ci/`, w tym w sondzie drainu, gdzie przy długim logu stwierdziłby,
że nie ma żadnej odpowiedzi 200. Dziś: pełne wyjście do zmiennej, potem
dopasowanie.

**Ogłoszenie wyniku na teście, który nie mógł zawieść.** Napisałem, że lokalny
BuildKit i ten z CI dają identyczny obraz, bo zgadzał się identyfikator. Drugi
build wziął jednak warstwy z pamięci podręcznej, więc zgodność niczego nie
dowodziła — przy buildach od zera różniły się czasy trzech katalogów. Dziś
test zawsze zawiera wariant, który *może* dać inny wynik: z pamięcią podręczną
kontra od zera, sterownik kontra sterownik.

**Walidator słabszy od tego, kto naprawdę decyduje.** `dependabot.yml`
przeszedł walidację względem schematu ze schemastore i tak to opisałem — a
Dependabot i tak go odrzucił: `semver-*-days` w cooldownie nie jest obsługiwane
dla `github-actions` i `docker`, czego schemat nie wyraża. Przez kilka godzin
na `main` stała konfiguracja, której Dependabot nie stosował. Zgoda słabszego
walidatora to nie dowód; dowodem jest odpowiedź systemu, który plik wykonuje.
Dziś po zmianie konfiguracji usługi zewnętrznej sprawdzam, czy ta usługa ją
przyjęła, zanim napiszę, że działa.

**Skrypt zakładający środowisko, w którym go napisałem.** `check-reproducible.sh`
wołał `uv`, którego zadanie `build` w CI nie ma — lokalnie było, więc przeszło.
Gorzej, że brak narzędzia skończył się komunikatem „build nie jest powtarzalny”:
awaria narzędzia wyglądała jak wynik. Dziś skrypt sprawdza swoje wymagania na
starcie, a kod wyjścia rozdziela wynik negatywny od awarii.
