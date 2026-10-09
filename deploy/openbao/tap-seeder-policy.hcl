# tap-seeder: the operator token behind `task secrets:put` / `secrets:import`.
# Writes and lists agent secrets; cannot read values back.
path "secret/data/tap/*" {
  capabilities = ["create", "update"]
}

path "secret/metadata/tap/*" {
  capabilities = ["read", "list"]
}

path "secret/metadata/tap" {
  capabilities = ["list"]
}

path "auth/token/renew-self" {
  capabilities = ["update"]
}

path "auth/token/lookup-self" {
  capabilities = ["read"]
}
