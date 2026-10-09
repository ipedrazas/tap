# tap-agent: what an agent namespace's External Secrets store may read.
#
# One policy for every agent. OpenBao fills in the namespace of the service
# account that logged in (auth/kubernetes-tap, role tap-agent), so agent-foo
# reads secret/tap/agent-foo/* and nothing else. ACCESSOR is replaced with the
# auth mount's accessor by onboard-tap.sh.
path "secret/data/tap/{{identity.entity.aliases.ACCESSOR.metadata.service_account_namespace}}/*" {
  capabilities = ["read"]
}
