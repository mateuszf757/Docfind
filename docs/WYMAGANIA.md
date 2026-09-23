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

## Porty poniżej 1024 dla Dockera rootless

**Sprawdzenie:** `sysctl -n net.ipv4.ip_unprivileged_port_start` — musi być ≤ 80.

**Dlaczego:** demon rootless nie ma uprawnień roota, więc nie otworzy portów 80
i 443, na które `deploy/k3d/cluster.yaml` mapuje load balancer. Domyślna granica
w Linuksie to 1024.

**Naprawa:**

```bash
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-rootless-ports.conf
sudo sysctl --system
```

To obniża granicę dla wszystkich procesów w systemie. Na jednoosobowym WSL-u
to akceptowalne; na współdzielonej maszynie lepiej zmapować 8080 i 8443
w `cluster.yaml` i zostawić granicę w spokoju.

Wszystkie trzy powyższe warunki sprawdza `ci/deploy-local.sh` przed utworzeniem
klastra i zgłasza je razem — każdy wymaga restartu WSL, więc zgłaszanie po
jednym kosztowałoby restart na każdy.

## Logi węzła, który nie wstał

Gdy k3d nie doczeka się gotowości serwera, **wycofuje klaster razem
z kontenerami węzłów, a więc i z ich logami** — jedynym dowodem przyczyny.
Żeby je zachować, trzeba je przechwytywać od startu kontenera:

```bash
( until docker ps --format '{{.Names}}' | grep -qx k3d-docfind-server-0; do sleep 0.3; done
  docker logs -f k3d-docfind-server-0 > server.log 2>&1 ) &
k3d cluster create --config deploy/k3d/cluster.yaml --image rancher/k3s:v1.36.4-k3s1 --timeout 180s
grep -E 'level=(fatal|error)' server.log
```

Tak znaleziona została przyczyna z poprzedniej sekcji. Pierwsza próba
odczytania logów po rollbacku trafiła już na nieistniejący kontener.

## Pamięć

**Sprawdzenie:** `free -h`

Etap 2 potrzebuje ~1,5 GB na trzy węzły k3d i dwie repliki API. Pełny stos
z docelowego planu (Elasticsearch, monitoring, ArgoCD, Vault) to ~7–8 GB bez
modelu LLM, który domyślnie stoi poza klastrem (decyzja 7). Limit pamięci WSL
ustawia się w `.wslconfig` kluczem `memory=`; domyślnie WSL dostaje połowę RAM-u
hosta.

## Porty 80 i 443 na hoście

**Sprawdzenie:** `ss -ltn | grep -E ':(80|443) '` — nic nie powinno słuchać.

Mapowane na load balancer k3d od utworzenia klastra, choć ingress wchodzi
dopiero na Etapie 3 — mapowania portów nie da się bezboleśnie dołożyć do
istniejącego klastra.

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
