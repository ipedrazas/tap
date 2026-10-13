# tap-signer: sign bundles, images and attestations with the pipeline key.
# The key lives in Transit (tap-transit/keys/tap-build), is not exportable and
# cannot be deleted; this policy may only sign with it and read its public half.
path "tap-transit/sign/tap-build" {
  capabilities = ["update"]
}

path "tap-transit/sign/tap-build/*" {
  capabilities = ["update"]
}

path "tap-transit/keys/tap-build" {
  capabilities = ["read"]
}
