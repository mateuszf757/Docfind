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
