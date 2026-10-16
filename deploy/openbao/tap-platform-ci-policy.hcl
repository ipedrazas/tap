# tap-platform-ci: what CI in ipedrazas/tap reads besides its signing policy
# (role tap-platform-ci on auth/jwt-github): its zot push credentials, which
# reach only the platform service images and tap/platform-manifests.
path "secret/data/tap-platform-ci/*" {
  capabilities = ["read"]
}
