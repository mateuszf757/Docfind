# Wymagania środowiska

Co musi być spełnione na maszynie, na której stawiasz lokalny klaster DOCFIND.
Każdy punkt ma sposób sprawdzenia — wymaganie, którego nie da się sprawdzić jedną
komendą, zostanie założone, a nie sprawdzone.

## Docker na cgroup v2

**Sprawdzenie:**

```bash
docker info --format '{{.CgroupVersion}}'   # musi być 2
stat -fc %T /sys/fs/cgroup                   # musi być cgroup2fs
```

**Dlaczego:** od Kubernetesa 1.35 kubelet domyślnie odmawia startu na cgroup v1.
Wewnątrz węzła k3d widać to jako
`kubelet is configured to not run on a host using cgroup v1`, ale sam k3d tego
nie zgłasza — widzi tylko, że serwer API nie odpowiada, i czeka na niego bez
końca. Bez kontroli w `dft cluster up` wyglądało to jak bardzo powolny start
i trwało ponad 10 minut, zanim ktokolwiek zajrzał do logów węzła.

Obejście przez flagę kubeleta `fail-cgroupv1=false` zostało sprawdzone i nie
wystarczyło — węzeł nadal nie wstał. Nawet gdyby zadziałało, opierałoby projekt
na trybie, który jest usuwany z Kubernetesa.

**Naprawa w WSL2:** WSL domyślnie montuje cgroup w trybie hybrydowym (kontrolery
v1 plus osobny montaż v2), więc Docker wybiera v1. W pliku
`%UserProfile%\.wslconfig` po stronie Windowsa:

```ini
[wsl2]
kernelCommandLine = cgroup_no_v1=all
```

Potem w PowerShellu `wsl --shutdown` i ponowne otwarcie terminala. W
`/etc/wsl.conf` musi być też `systemd=true` w sekcji `[boot]`.

## Kontroler cpuset widoczny w kontenerze

**Sprawdzenie:**

```bash
docker run --rm --entrypoint /bin/cat rancher/k3s:v1.36.4-k3s1 /sys/fs/cgroup/cgroup.controllers
# musi zawierać cpuset
```

**Dlaczego:** k3s w kontenerze potrzebuje kontrolera `cpuset` i bez niego kończy
się błędem `failed to find cpuset cgroup (v2)`. Samo cgroup v2 na hoście nie
wystarcza — liczy się to, co faktycznie dociera do kontenera, i dlatego
sprawdzenie jest robione z jego wnętrza.

Na tej maszynie Docker działa w trybie **rootless**: kontenery trafiają pod
`user.slice/user-1000.slice/user@1000.service/…`, a nie pod `system.slice`.
systemd domyślnie deleguje do sesji użytkownika tylko `cpu memory pids`. Host
i `system.slice` mają `cpuset`, ale na granicy `user.slice` kontroler znika.
Rozpoznać to można po ścieżce cgroup dowolnego kontenera:

```bash
cat /proc/$(docker inspect -f '{{.State.Pid}}' <kontener>)/cgroup
docker info --format '{{.SecurityOptions}}'   # name=rootless
```

**Naprawa:**

```bash
sudo mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\nDelegate=cpu cpuset io memory pids\n' \
  | sudo tee /etc/systemd/system/user@.service.d/delegate.conf
sudo systemctl daemon-reload
```

Delegacja obejmuje sesje uruchomione po zmianie, więc potem `wsl --shutdown`.

## Wolne porty 6550, 8080 i 8443 na loopbacku

**Sprawdzenie:** `ss -ltn 'sport = :6550 or sport = :8080 or sport = :8443'` —
nic nie powinno słuchać.

Klaster wystawia na hosta API Kubernetesa (6550) oraz load balancer pod ingress
z Etapu 3 (8080 → 80, 8443 → 443), wyłącznie na `127.0.0.1`. Szczegóły
i uzasadnienie: decyzja 17. Klastry pozostałych środowisk mają własne porty
z definicji w `deploy/environments/`: `ci` 6551/8081/8444, `staging`
6552/8082/8445 — wszystkie mogą stać obok siebie.

**Czego świadomie nie robimy.** Docker rootless nie otworzy portów 80 i 443.
Obejściem, które podaje dokumentacja Dockera, jest obniżenie
`net.ipv4.ip_unprivileged_port_start` (nawet do 0). Otwiera to jednak dla
każdego procesu bez roota **cały zakres od tej wartości do 1023**, a nie tylko
porty klastra. Każdy proces użytkownika mógłby wtedy zająć port usługi
systemowej, zanim wstanie ona sama. Węższe obejście, `setcap cap_net_bind_service`
na binarce rootlesskit, znika przy każdej aktualizacji pakietu i dryfuje
niezauważenie. Środowisko deweloperskie nie jest powodem do rozluźniania
zabezpieczeń hosta. Wymaganie sudo odcięłoby też każdego, kto pracuje na
firmowym laptopie bez uprawnień administratora.

Jeśli sysctl został już zmieniony, należy go wycofać:

```bash
sudo rm /etc/sysctl.d/99-rootless-ports.conf
sudo sysctl -w net.ipv4.ip_unprivileged_port_start=1024
```

Wszystkie trzy powyższe warunki sprawdza `dft cluster up` przed utworzeniem
klastra i zgłasza je razem — dwa z nich wymagają restartu WSL, więc zgłaszanie
po jednym kosztowałoby restart na każdy.

## Logi węzła, który nie wstał

Gdy k3d nie doczeka się gotowości węzła, **wycofuje klaster razem z kontenerami
węzłów, a więc i z ich logami** — jedynym dowodem przyczyny. Przy ręcznym
przechwytywaniu przyczyna ginęła dwa razy: raz, bo przechwytywany był tylko
serwer, a padł agent; drugi raz, bo `docker logs -f` uruchomiony na kontenerze
w stanie `Created` kończy się od razu z pustym plikiem.

Dlatego `dft cluster up` robi to sam: od chwili startu każdego kontenera
węzła zapisuje jego log do `~/.cache/docfind/k3d-create-<czas>/` i przy porażce
wypisuje z nich błędy. Logi zostają na dysku niezależnie od wyniku.

## Kubelet w przestrzeni nazw użytkownika (Docker rootless)

**Sprawdzenie:** `docker info --format '{{.SecurityOptions}}'` — jeśli zawiera
`name=rootless`, `dft cluster up` dokłada kubeletowi bramkę
`KubeletInUserNamespace=true`.

**Dlaczego:** w rootless kubelet nie może zapisać globalnych sysctli jądra
i kończy się `Failed to start ContainerManager: open
/proc/sys/vm/overcommit_memory: permission denied`. Bramka każe mu ten błąd
pominąć. W Kubernetesie 1.36 jest to funkcja **alfa** (decyzja 19) i kubelet
ostrzega o tym przy starcie.

Objaw był mylący: serwer zgłaszał `k3s is up and running`, a zaraz potem proces
znikał, podczas gdy kontener dalej stał, bo trzyma go skrypt startowy k3d.
Agenci widzieli wtedy `connection reset` i wyglądało to jak problem sieci
albo DNS — a było to zapukanie do nieistniejącego procesu.

## Limit instancji inotify

**Sprawdzenie:** `sysctl -n fs.inotify.max_user_instances` — co najmniej 512.

**Dlaczego:** przy Dockerze rootless wszystkie procesy we wszystkich kontenerach
— węzły k3s, kubelety, containerd, każdy pod — działają na hoście jako jeden
użytkownik i dzielą jego limit instancji inotify. Domyślne 128 przy klastrze
z Etapu 3 było zajęte w 99 (głównie containerd-shim i k3s), a rolling update
proxy podwaja na chwilę liczbę podów Envoy. Envoy bez inotify kończy się
SIGSEGV: `assert failure: inotify_fd_ >= 0. Consider increasing value of
fs.inotify.max_user_watches and/or fs.inotify.max_user_instances`.

**Naprawa:**

```bash
echo 'fs.inotify.max_user_instances=512' | sudo tee /etc/sysctl.d/99-inotify.conf
sudo sysctl --system
```

512 to wartość, którą dokumentacja kind podaje dla klastrów w kontenerach —
około dwukrotny zapas ponad pełny stos z planu. To limit zasobu jądra, nie
granica uprawnień: podnosi pamięć, jaką użytkownik może zająć na struktury
inotify, a nie to, co może zrobić. `dft cluster up` sprawdza go przy każdym
wdrożeniu i ostrzega powyżej 80% zużycia. Klaster z Etapu 3 zajmuje ~96
instancji, więc dev i staging razem mieszczą się w 512 z zapasem; klaster CI
postawiony lokalnie obok nich też.

## Token API Cloudflare (Let's Encrypt)

Potrzebny tylko dla wydawców Let's Encrypt; własne CA działa bez niego.

**Uprawnienia** (Cloudflare → My Profile → API Tokens → Create Token):
`Zone → DNS → Edit` i `Zone → Zone → Read`, a w `Zone Resources` tylko
`Include → Specific zone → <strefa>`. Token do wszystkich stref pozwoliłby po
wycieku przejąć każdą domenę na koncie.

**Zapisanie w klastrze:**

```bash
DOCFIND_ACME_ZONE=<strefa> ./bin/mise run dns-token
```

Skrypt czyta token bez echa (albo ze standardowego wejścia, np. z menedżera
haseł), sprawdza go w API Cloudflare — także czy widzi strefę — i dopiero
wtedy zapisuje Secret. Token nie trafia do gita, historii powłoki ani do
argumentów procesów.

**Ustawienia klastra** — host, adres e-mail konta ACME, strefa i wydawca —
w `mise.local.toml` (poza gitem):

```toml
[env]
DOCFIND_HOSTNAME = "local.<strefa>"
DOCFIND_ACME_EMAIL = "<adres>"
DOCFIND_ACME_ZONE = "<strefa>"
DOCFIND_TLS_ISSUER = "letsencrypt-staging"
```

Dostęp z przeglądarki w Windows: rekord `A local.<strefa> → 127.0.0.1`
w Cloudflare, bez proxy (DNS only), i adres `https://local.<strefa>:8443`.

## Pamięć

**Sprawdzenie:** `free -h`

Etap 2 potrzebuje ~1,5 GB na trzy węzły k3d i dwie repliki API. Pełny stos
z docelowego planu (Elasticsearch, monitoring, ArgoCD, Vault) to ~7–8 GB bez
modelu LLM, który domyślnie stoi poza klastrem (decyzja 7). Limit pamięci WSL
ustawia się w `.wslconfig` kluczem `memory=`; domyślnie WSL dostaje połowę RAM-u
hosta.

Staging to drugi klaster na tej samej maszynie (decyzja 35), więc pamięć
liczy się dwa razy. Do Etapu 5 oba klastry mieszczą się w 13 GiB; od Etapu 6
(Elasticsearch) staging dostaje okrojony stos albo jest wyłączany na czas
pracy nad dev: `k3d cluster stop docfind-staging`. Węzły k3d widzą całą
pamięć WSL, więc przepełnienie kończy się OOM killerem hosta, a nie eksmisją
podów.

## Narzędzia

**Instalacja i sprawdzenie:** `./bin/mise install`

`kubectl`, `k3d`, `helm`, `kubeconform`, `shellcheck`, `actionlint`, `zizmor`,
`gh`, `cmctl` i `go` (toolchain narzędzi z `tools/`) w wersjach z `mise.toml`,
weryfikowane względem sum SHA-256 dla każdej
platformy zapisanych w `mise.lock` (`locked = true` odmawia instalacji
czegokolwiek spoza lockfile'a). `bin/mise` pobiera samo mise w przypiętej
wersji i sprawdza jego sumę; wszystko ląduje w `.mise/` w repozytorium — bez
sudo, bez menedżera pakietów systemu i bez zmian w konfiguracji powłoki.
Drugi bieg niczego nie pobiera.

Poza mise zostają: Docker i `uv` (wersja z Dockerfile — `dft check versions`
ostrzega, gdy lokalna jest inna). Systemowy `python3` nie jest potrzebny:
narzędzia są w Go, a Python aplikacji uruchamia `uv`. Go z systemu nie jest potrzebne ani używane:
`GOTOOLCHAIN=local` w `mise.toml` sprawia, że `go` nie pobiera innego
toolchainu, nawet gdy zażąda go `tools/go.mod`.

**Detektor wyścigów (`go test -race`)** wymaga kompilatora C (cgo). Bez gcc
testy biegną bez niego, z ostrzeżeniem; w CI (`CI=true`) brak kompilatora
kończy bramkę kodem 2. Instalacja gcc wymaga sudo, więc lokalnie nie jest
wymagana — wyścig wyjdzie najpóźniej w zadaniu `test` w CI.

**Kubeconfig:** klaster zapisuje dane dostępowe do `.cache/kubeconfig`
w repozytorium, nie do `~/.kube/config`. `dft` bierze ścieżkę i kontekst
z definicji środowiska (`deploy/environments/dev.yaml`) i podaje je jawnie,
a `mise.toml` ustawia `KUBECONFIG` dla `./bin/mise exec`. Klastry `ci`
i `staging` mają własne pliki (`.cache/kubeconfig-ci`,
`.cache/kubeconfig-staging`), wybierane przez `MISE_ENV=ci` albo
`MISE_ENV=staging`. Kontekst `k3d-docfind` dopisany wcześniej do
globalnego kubeconfigu można usunąć:
`kubectl config delete-context k3d-docfind && kubectl config delete-cluster k3d-docfind && kubectl config delete-user admin@k3d-docfind`.

## Zegar

**Sprawdzenie:** `date -u`

WSL po hibernacji hosta potrafi mieć przesunięty zegar. Psuje to certyfikaty,
tokeny, szeregi czasowe i pomiary czasu — jeden pomiar `docker stop` dał przez
to wynik ujemny. Gdy po przerwie w pracy dzieje się coś dziwnego, pierwszą
komendą jest `date -u`.
