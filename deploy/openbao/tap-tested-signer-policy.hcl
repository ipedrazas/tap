# tap-tested-signer: the second signature admission requires, which marks a
# bundle whose fixtures passed (task agent:test only). Kept apart from
# tap-signer so a CI runner can hold one without the other.
path "tap-transit/sign/tap-tested" {
  capabilities = ["update"]
}

path "tap-transit/sign/tap-tested/*" {
  capabilities = ["update"]
}

path "tap-transit/keys/tap-tested" {
  capabilities = ["read"]
}
