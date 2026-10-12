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
