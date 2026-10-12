# `dft dns-token` i migracja tokenu przy `cluster up`: zapis tokenu API
# Cloudflare i odczyt metadanych (numer wersji, bez wartości) — bez prawa
# odczytu samego tokenu (decyzja 43).

path "docfind/data/platform/cloudflare" {
  capabilities = ["create", "update"]
}
path "docfind/metadata/platform/cloudflare" {
  capabilities = ["read"]
}

path "auth/token/revoke-self" {
  capabilities = ["update"]
}
