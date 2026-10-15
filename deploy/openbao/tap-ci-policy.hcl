# tap-ci: what CI in ipedrazas/tap-agents reads besides its signing policy
# (roles tap-ci-build and tap-ci-tested on auth/jwt-github): the zot push
# credentials and Kubernetes API details, and a short-lived token for the
# tap-ci-runner service account (fixture Jobs in tap-ci only).
path "secret/data/tap-ci/*" {
  capabilities = ["read"]
}

path "tap-kubernetes/creds/tap-ci-runner" {
  capabilities = ["update"]
}
