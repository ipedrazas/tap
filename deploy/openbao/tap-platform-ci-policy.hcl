# tap-platform-ci: what CI in ipedrazas/tap reads besides its signing policy
# (role tap-platform-ci on auth/jwt-github): its zot push credentials, which
# reach only the platform service images and tap/platform-manifests, the
# Kubernetes API details, and a short-lived token for tap-platform-ci-runner
# (read the platform Kustomization only; deploy/ci/platform-rbac.yaml).
path "secret/data/tap-platform-ci/*" {
  capabilities = ["read"]
}

path "tap-kubernetes/creds/tap-platform-ci-runner" {
  capabilities = ["update"]
}
