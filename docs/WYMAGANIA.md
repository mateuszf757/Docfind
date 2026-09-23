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
końca. Bez kontroli w `ci/deploy-local.sh` wyglądało to jak bardzo powolny start
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
i uzasadnienie: decyzja 17.

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

Wszystkie trzy powyższe warunki sprawdza `ci/deploy-local.sh` przed utworzeniem
klastra i zgłasza je razem — dwa z nich wymagają restartu WSL, więc zgłaszanie
po jednym kosztowałoby restart na każdy.

## Logi węzła, który nie wstał

Gdy k3d nie doczeka się gotowości węzła, **wycofuje klaster razem z kontenerami
węzłów, a więc i z ich logami** — jedynym dowodem przyczyny. Przy ręcznym
przechwytywaniu przyczyna ginęła dwa razy: raz, bo przechwytywany był tylko
serwer, a padł agent; drugi raz, bo `docker logs -f` uruchomiony na kontenerze
w stanie `Created` kończy się od razu z pustym plikiem.

Dlatego `ci/deploy-local.sh` robi to sam: od chwili startu każdego kontenera
węzła zapisuje jego log do `~/.cache/docfind/k3d-create-<czas>/` i przy porażce
wypisuje z nich błędy. Logi zostają na dysku niezależnie od wyniku.

## Kubelet w przestrzeni nazw użytkownika (Docker rootless)

**Sprawdzenie:** `docker info --format '{{.SecurityOptions}}'` — jeśli zawiera
`name=rootless`, `ci/deploy-local.sh` dokłada kubeletowi bramkę
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

## Pamięć

**Sprawdzenie:** `free -h`

Etap 2 potrzebuje ~1,5 GB na trzy węzły k3d i dwie repliki API. Pełny stos
z docelowego planu (Elasticsearch, monitoring, ArgoCD, Vault) to ~7–8 GB bez
modelu LLM, który domyślnie stoi poza klastrem (decyzja 7). Limit pamięci WSL
ustawia się w `.wslconfig` kluczem `memory=`; domyślnie WSL dostaje połowę RAM-u
hosta.

## Narzędzia

**Instalacja i sprawdzenie:** `ci/install-tools.sh`

`kubectl`, `k3d`, `helm` i `kubeconform` w wersjach przypiętych w `ci/lib.sh`,
weryfikowane względem sum SHA-256 zapisanych w repozytorium. Skrypt nie używa
sudo ani menedżera pakietów systemu i jest idempotentny — drugi bieg niczego nie
pobiera.

`ci/run-tests.sh` potrzebuje tylko Dockera i `uv`: shellcheck, helm i kubeconform
biegną w nim z przypiętych obrazów, tak samo lokalnie i w CI.

## Zegar

**Sprawdzenie:** `date -u`

WSL po hibernacji hosta potrafi mieć przesunięty zegar. Psuje to certyfikaty,
tokeny, szeregi czasowe i pomiary czasu — jeden pomiar `docker stop` dał przez
to wynik ujemny. Gdy po przerwie w pracy dzieje się coś dziwnego, pierwszą
komendą jest `date -u`.
