# tap-platform: ESO in tap-system pushing the platform secrets every agent
# reads (model key, trigger token, sessions write key) to secret/tap-platform/.
# Outside secret/tap/, so neither the console nor the seeder can touch them.
path "secret/data/tap-platform/*" {
  capabilities = ["create", "update", "read", "delete"]
}

path "secret/metadata/tap-platform/*" {
  capabilities = ["create", "update", "read", "delete"]
}
