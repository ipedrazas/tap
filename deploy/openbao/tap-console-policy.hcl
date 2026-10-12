# tap-console: what the console may do when a team member sets an agent
# secret. Same reach as tap-seeder: write values and read metadata (to show
# which secrets are set), never read values back. The console also checks
# that each path is inside the agent's own <prefix>/<namespace>/.
path "secret/data/tap/*" {
  capabilities = ["create", "update"]
}

path "secret/metadata/tap/*" {
  capabilities = ["read"]
}
