# Administracja platformy: `dft` doprowadza OpenBao do konfiguracji z gita
# (deploy/platform/openbao/config.yaml).
#
# Kto może zmieniać polityki, może nadać sobie każdą — ta polityka jest więc
# w praktyce równoważna root. Dlatego token żyje 15 minut i wydaje go tylko
# rola platform-admin dla konta openbao-admin (decyzja 42). Bez dostępu do
# wartości sekretów: tych dft nie czyta, a zapisy (token DNS, sekrety
# aplikacji) dostają osobne, wąskie polityki.

path "sys/audit" {
  capabilities = ["read", "sudo"]
}
path "sys/audit/*" {
  capabilities = ["create", "update", "sudo"]
}

path "sys/mounts" {
  capabilities = ["read"]
}
path "sys/mounts/*" {
  capabilities = ["create", "read", "update"]
}

path "sys/auth" {
  capabilities = ["read"]
}
path "sys/auth/*" {
  capabilities = ["create", "read", "update", "sudo"]
}

path "sys/policies/acl" {
  capabilities = ["list"]
}
path "sys/policies/acl/*" {
  capabilities = ["create", "read", "update", "delete"]
}

path "auth/kubernetes/config" {
  capabilities = ["create", "read", "update"]
}
path "auth/kubernetes/role" {
  capabilities = ["list"]
}
path "auth/kubernetes/role/*" {
  capabilities = ["create", "read", "update", "delete"]
}

# Stan klastra Raft — bramka sprawdza, że wszystkie repliki głosują.
path "sys/storage/raft/configuration" {
  capabilities = ["read"]
}

# Rola nie ma polityki default, więc odwołanie własnego tokenu po pracy
# trzeba nadać wprost.
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
