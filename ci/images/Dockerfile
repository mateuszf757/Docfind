# Obrazy środowiska CI (decyzja 78).
#
# light — system jak runner GitHuba (Ubuntu 24.04), podstawy i mise
# w przypiętej wersji z launchera repozytorium. Uruchamia zadania mise
# w kontenerze, z repozytorium zamontowanym pod tą samą ścieżką
# (`./bin/mise run ci:container -- <zadanie>`): narzędzia biorą się z .mise
# repozytorium, a obraz daje to, czego brakuje hostowi — kompilator C dla
# `go test -race` bez sudo.
#
# full — narzędzia, moduły i kompilacja Go w obrazie — dopiero na progi
# z decyzji 69.
FROM ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac AS light

# Pakiety z migawki archiwum Ubuntu o ustalonej dacie: ten sam Dockerfile
# daje te same wersje pakietów, dopóki data się nie zmieni. Migawka idzie
# przez HTTPS, a obraz bazowy nie ma certyfikatów CA — najpierw
# ca-certificates ze zwykłego archiwum, potem wszystko z migawki. gcc
# i libc6-dev — dla cgo, którego wymaga detektor wyścigów.
ARG UBUNTU_SNAPSHOT=20261015T000000Z
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && apt-get update --snapshot "$UBUNTU_SNAPSHOT" \
 && apt-get install -y --no-install-recommends --snapshot "$UBUNTU_SNAPSHOT" \
      bash ca-certificates curl gcc git libc6-dev tar xz-utils zstd \
 && rm -rf /var/lib/apt/lists/*

# mise przez launcher repozytorium: wersja i suma z bin/mise, te same co
# lokalnie i w CI. Launcher instaluje mise obok siebie, w /opt/docfind-ci/.mise;
# w PATH sama binarka — launcher pod innym katalogiem instalowałby ją od nowa.
COPY bin/mise /opt/docfind-ci/bin/mise
RUN /opt/docfind-ci/bin/mise --version \
 && ln -s /opt/docfind-ci/.mise/mise-* /usr/local/bin/mise
