# `dft`: wygenerowanie brakujących sekretów aplikacji przy wdrożeniu
# i bramka rotacji — zapis i metadane (numer wersji) pod app/, bez odczytu
# wartości (decyzja 44).

path "docfind/data/app/*" {
  capabilities = ["create", "update"]
}
path "docfind/metadata/app/*" {
  capabilities = ["read"]
}

path "auth/token/revoke-self" {
  capabilities = ["update"]
}
