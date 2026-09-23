#!/usr/bin/env bash
# Warunki zakończenia Etapu 1, zapisane wykonywalnie.
#
#   ci/check-runtime.sh api
#
# Sensem warunku zakończenia jest to, że da się go sprawdzić bez oceniającego.
# Warunek opisany w pliku markdown nie jest sprawdzany; skrypt jest.

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
# shellcheck source=ci/lib.sh
source "$repo_root/ci/lib.sh"

service="${1:-api}"
image="$(df_image_name "$service"):$(df_docker_tag)"

MAX_IMAGE_MB=200
MAX_SHUTDOWN_SECONDS=1.0
# Zmierzony koszt własny uvicorna na Alpine mieści się w 0,37-0,50 s. Próg
# ustawiony na granicy dawałby test migoczący co drugi przebieg, a test, który
# zawodzi losowo, uczy ignorowania czerwonego wyniku. Próg ma rozróżniać awarię,
# a ta wygląda inaczej: proces bez handlera SIGTERM czeka pełne 10 s do SIGKILL.
MAX_OWN_SHUTDOWN_SECONDS=0.75

failures=0

fail() {
  echo "NIESPEŁNIONE: $*" >&2
  failures=$((failures + 1))
}

df_log "sprawdzany obraz: $image"

# --- rozmiar obrazu ---------------------------------------------------------
size_bytes=$(docker image inspect "$image" --format '{{.Size}}')
size_mb=$((size_bytes / 1000 / 1000))
if (( size_mb <= MAX_IMAGE_MB )); then
  df_log "rozmiar obrazu: ${size_mb} MB (limit ${MAX_IMAGE_MB} MB)"
else
  fail "rozmiar obrazu ${size_mb} MB przekracza limit ${MAX_IMAGE_MB} MB"
fi

# --- odmowa startu bez konfiguracji ----------------------------------------
if output=$(docker run --rm "$image" 2>&1); then
  fail "kontener wstał bez konfiguracji zamiast odmówić"
elif grep -q "BŁĄD KONFIGURACJI" <<<"$output"; then
  df_log "brak konfiguracji: czytelna odmowa startu"
else
  fail "odmowa startu bez czytelnego komunikatu: $output"
fi

# --- czas zamknięcia --------------------------------------------------------
config_dir=$(mktemp -d)
chmod 755 "$config_dir"
cp "$repo_root/deploy/config/app.yml.example" "$config_dir/app.yml"
chmod 644 "$config_dir/app.yml"
trap 'rm -rf "$config_dir"' EXIT

# Mierzymy dwie rzeczy. Całkowity czas `docker stop` to literalny warunek
# z Etapu 1, ale zawiera narzut demona Dockera zależny od maszyny, nie od
# naszego kodu. Dlatego mierzymy narzut osobno i sprawdzamy także koszt
# własny aplikacji — to jest liczba, na którą mamy wpływ.
#
# Zegar monotoniczny, nie ścienny: w WSL po hibernacji ten drugi potrafi
# skoczyć i dać ujemny czas trwania.
read -r baseline_seconds shutdown_seconds < <(
  IMAGE="$image" CONFIG_DIR="$config_dir" python3 - <<'PYEOF'
import os
import statistics
import subprocess
import time
import urllib.request

image = os.environ["IMAGE"]
config_dir = os.environ["CONFIG_DIR"]
REPEATS = 5


def stop_after(name: str, run_args: list[str], wait_for_http: bool) -> float:
    subprocess.run(
        ["docker", "run", "-d", "--rm", "--name", name, *run_args],
        capture_output=True,
        check=True,
    )
    if wait_for_http:
        for _ in range(50):
            try:
                urllib.request.urlopen("http://127.0.0.1:18099/healthz", timeout=1).read()
                break
            except OSError:
                time.sleep(0.2)
    else:
        time.sleep(0.8)

    started = time.monotonic()
    subprocess.run(["docker", "stop", name], capture_output=True, check=True)
    return time.monotonic() - started


# Odniesienie: PID 1, który sam zakłada handler SIGTERM i kończy się
# natychmiast. Bez własnego handlera jądro nie dostarczyłoby sygnału do
# PID 1 wcale i `docker stop` czekałby pełne 10 s do SIGKILL.
handler = (
    "import signal,sys,time; "
    "signal.signal(signal.SIGTERM, lambda *a: sys.exit(0)); "
    "time.sleep(300)"
)
baseline = statistics.median(
    stop_after(f"df-base-{i}", ["--entrypoint", "python", image, "-c", handler], False)
    for i in range(REPEATS)
)
total = statistics.median(
    stop_after(
        f"df-stop-{i}",
        ["-p", "18099:8000", "-v", f"{config_dir}:/app/config:ro", image],
        True,
    )
    for i in range(REPEATS)
)
print(f"{baseline:.3f} {total:.3f}")
PYEOF
)

own_seconds=$(awk "BEGIN {printf \"%.3f\", $shutdown_seconds - $baseline_seconds}")
df_log "narzut samego docker stop: ${baseline_seconds} s"

if awk "BEGIN {exit !($own_seconds <= $MAX_OWN_SHUTDOWN_SECONDS)}"; then
  df_log "koszt własny zamykania: ${own_seconds} s (limit ${MAX_OWN_SHUTDOWN_SECONDS} s)"
else
  fail "aplikacja zamyka się ${own_seconds} s ponad narzut, limit to ${MAX_OWN_SHUTDOWN_SECONDS} s"
fi

# Całkowity czas raportujemy, ale nie na nim opieramy werdykt. Narzut demona
# Dockera zmierzony na tej maszynie waha się między 0,5 a 0,8 s w kolejnych
# przebiegach — kryterium oparte na sumie mówiłoby o obciążeniu WSL-a, nie
# o obsłudze SIGTERM w naszym kodzie. Werdykt opiera się na koszcie własnym.
if awk "BEGIN {exit !($shutdown_seconds <= $MAX_SHUTDOWN_SECONDS)}"; then
  df_log "docker stop razem: ${shutdown_seconds} s (mediana z 5)"
else
  df_log "UWAGA: docker stop razem ${shutdown_seconds} s, powyżej orientacyjnego progu ${MAX_SHUTDOWN_SECONDS} s"
  df_log "       z czego ${baseline_seconds} s to narzut Dockera, a ${own_seconds} s nasz kod"
fi

if (( failures > 0 )); then
  echo "" >&2
  echo "Warunki zakończenia Etapu 1: $failures niespełnionych." >&2
  exit 1
fi

df_log "wszystkie warunki zakończenia Etapu 1 spełnione"
