# tap-agent: what an agent namespace's External Secrets store may read.
#
# One policy for every agent. OpenBao fills in the namespace of the service
# account that logged in (auth/kubernetes-tap, role tap-agent), so agent-foo
# reads secret/tap/agent-foo/* and nothing else. ACCESSOR is replaced with the
# auth mount's accessor by onboard-tap.sh.
path "secret/data/tap/{{identity.entity.aliases.ACCESSOR.metadata.service_account_namespace}}/*" {
  capabilities = ["read"]
}

# Platform secrets every agent needs (secrets.sharedPrefix), pushed from
# tap-system by deploy/platform/openbao-platform.yaml. Named one by one:
# nothing else under tap-platform/ is for agents.
path "secret/data/tap-platform/model-credentials" {
  capabilities = ["read"]
}

path "secret/data/tap-platform/trigger-token" {
  capabilities = ["read"]
}

path "secret/data/tap-platform/sessions-credentials" {
  capabilities = ["read"]
}

# The namespace's own egress key (secrets.egressPrefix), generated in the
# namespace and written here by its PushSecret; the egress proxy reads it.
# ESO keeps its ownership marker in the metadata.
path "secret/data/tap-egress/{{identity.entity.aliases.ACCESSOR.metadata.service_account_namespace}}" {
  capabilities = ["create", "update", "read"]
}

path "secret/metadata/tap-egress/{{identity.entity.aliases.ACCESSOR.metadata.service_account_namespace}}" {
  capabilities = ["create", "update", "read"]
}
