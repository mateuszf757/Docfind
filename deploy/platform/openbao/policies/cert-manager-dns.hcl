# External Secrets Operator w przestrzeni cert-managera: tylko odczyt tokenu
# API Cloudflare dla wydawców ACME (decyzja 43). Żadnej listy ścieżek
# i żadnego innego sekretu — przejęte konto openbao-secrets czyta tylko to.

path "docfind/data/platform/cloudflare" {
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
