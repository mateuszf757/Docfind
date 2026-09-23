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

## 11. ingress-nginx zamiast Traefika i Gateway API

**Wybieram najpowszechniejsze.** k3d domyślnie dostarcza Traefika, a Gateway API jest kierunkiem,
w którym ekosystem idzie. Wybieram ingress-nginx, bo tego najczęściej dotknę w pracy.

**Co tracę:** uczę się interfejsu, który jest na wylocie, i wyłączam komponent dostarczany
z dystrybucją, zamiast używać gotowego.

**Kiedy zmieniam zdanie:** gdy Gateway API stanie się domyślne w dokumentacji Kubernetesa —
wtedy migracja samego wejścia jest dobrym, wyizolowanym ćwiczeniem.

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

**Co tracę:** czytelność — `@sha256:edad48e1…` nic nie mówi bez komentarza —
i darmowe łatki bezpieczeństwa, które przy tagu przychodziły same. Tę drugą
stratę pokrywa Dependabot (akcje, bazy w Dockerfile, uv.lock), ale **nie**
przypięcia w `ci/lib.sh`: k3s, narzędzia, charty i obrazy narzędzi CI
aktualizuję ręcznie, razem z sumami.

**Kiedy zmieniam zdanie:** nigdy dla samego przypinania. Ręczne aktualizacje
z `ci/lib.sh` przeniosę do Renovate z regułami regex, gdy zaczną zalegać.

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
Wynik: identyczny obraz przy dwóch buildach od zera **i** przy dwóch różnych
sterownikach BuildKit (lokalnym i tym z CI). `ci/check-reproducible.sh`
sprawdza to w każdym pipeline'ie.

**Co tracę:** trzy rzeczy. Pole `built_at` w `/version` zmieniło nazwę na
`source_date`, bo podaje czas commita, a nie budowania — zmiana kontraktu.
Lokalne obrazy nie mają atestacji pochodzenia, bo ta z natury zmienia digest
co build; publikowane do rejestru mają. Pakiet `docfind_api` nie jest
zainstalowany, więc nie ma go w `importlib.metadata` — kod nie korzysta
z tego, ale narzędzie do inwentaryzacji zależności go nie zobaczy.

**Kiedy zmieniam zdanie:** gdy uv przestanie zapisywać `uv_cache.json`
z ctime albo pozwoli to wyłączyć — wtedy wracam do instalacji koła, bo to
zwyklejszy układ dla każdego, kto otworzy obraz.

---

## Czego bym dziś nie powtórzył

Najważniejsza część tego dokumentu i najrzadziej przygotowana — sekcja pusta
na koniec projektu oznacza, że projekt niczego nie nauczył.

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
