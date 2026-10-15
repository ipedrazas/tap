# tap-egress-proxy: reads every agent's egress key, which each agent namespace
# pushes to secret/tap-egress/<namespace>. Read only.
path "secret/data/tap-egress/*" {
  capabilities = ["read"]
}
