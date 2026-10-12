# External Secrets Operator w przestrzeni aplikacji (SecretStore openbao
# z charta docfind): odczyt sekretów aplikacji — tylko pod app/, bez listy
# ścieżek i bez sekretów platformy (decyzja 44).

path "docfind/data/app/*" {
  capabilities = ["read"]
}

# Rola nie ma polityki default: ESO sprawdza własny token przy walidacji
# SecretStore i odwołuje go po pracy.
path "auth/token/lookup-self" {
  capabilities = ["read"]
}
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
