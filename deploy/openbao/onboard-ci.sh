#!/bin/sh
#
# Let CI in the agents repo sign and run fixtures without stored credentials.
# Idempotent. Run it with `task ci:onboard`, which starts this script in a pod
# in tap-system and passes the admin token and the zot CI password on stdin
# (one per line). It sets up:
#   - auth/jwt-github: GitHub Actions OIDC tokens (audience "openbao")
#   - role tap-ci-build: GH_REPO's WORKFLOW on main; policies tap-signer, tap-ci
#   - role tap-ci-tested: the same, in environment "tested" only; policies
#     tap-signer (the attestation), tap-tested-signer, tap-ci. No other job
#     can sign as tested.
#   - tap-kubernetes/: Kubernetes secrets engine minting short-lived tokens for
#     tap-ci/tap-ci-runner (deploy/ci/rbac.yaml), as openbao-ci-issuer
#   - secret/tap-ci/zot (CI push user) and secret/tap-ci/kube (API server, CA)
#
# Inputs: BAO_ADDR, K8S_HOST, GH_REPO, WORKFLOW (env); admin token and zot
# password on stdin; the issuer token and cluster CA at /run/issuer/{token,ca.crt};
# policies in /policies.
set -eu

: "${BAO_ADDR:?}" "${K8S_HOST:?}" "${GH_REPO:?}" "${WORKFLOW:?}"
read -r BAO_TOKEN
read -r ZOT_PASSWORD
export BAO_ADDR BAO_TOKEN
[ -n "$ZOT_PASSWORD" ] || { echo "ERROR: no zot CI password on stdin" >&2; exit 1; }

bao token lookup >/dev/null || { echo "ERROR: the token was rejected" >&2; exit 1; }

echo "==> JWT auth at auth/jwt-github (GitHub Actions OIDC)" >&2
bao auth enable -path=jwt-github jwt >/dev/null 2>&1 && echo "    enabled" >&2 || echo "    already enabled" >&2
bao write auth/jwt-github/config \
  oidc_discovery_url=https://token.actions.githubusercontent.com \
  bound_issuer=https://token.actions.githubusercontent.com >/dev/null

echo "==> Policy tap-ci" >&2
bao policy write tap-ci /policies/tap-ci-policy.hcl >/dev/null

workflow_ref="$GH_REPO/.github/workflows/$WORKFLOW@refs/heads/main"
role() { # name, policies, extra bound claims (JSON members)
  cat <<EOF | bao write "auth/jwt-github/role/$1" - >/dev/null
{
  "role_type": "jwt",
  "user_claim": "sub",
  "bound_audiences": ["openbao"],
  "bound_claims_type": "string",
  "bound_claims": {
    "repository": "$GH_REPO",
    "ref": "refs/heads/main",
    "job_workflow_ref": "$workflow_ref"$3
  },
  "token_policies": "$2",
  "token_ttl": "30m",
  "token_max_ttl": "1h"
}
EOF
  echo "    role $1: $2" >&2
}
echo "==> Roles for $workflow_ref" >&2
role tap-ci-build tap-signer,tap-ci ""
role tap-ci-tested tap-signer,tap-tested-signer,tap-ci ', "environment": "tested"'

echo "==> Kubernetes secrets engine at tap-kubernetes/" >&2
bao secrets enable -path=tap-kubernetes kubernetes >/dev/null 2>&1 && echo "    enabled" >&2 || echo "    already enabled" >&2
bao write tap-kubernetes/config \
  kubernetes_host="$K8S_HOST" \
  kubernetes_ca_cert=@/run/issuer/ca.crt \
  service_account_jwt=@/run/issuer/token \
  disable_local_ca_jwt=true >/dev/null
bao write tap-kubernetes/roles/tap-ci-runner \
  service_account_name=tap-ci-runner \
  allowed_kubernetes_namespaces=tap-ci \
  token_default_ttl=30m token_max_ttl=1h >/dev/null

echo "==> CI settings at secret/tap-ci/" >&2
printf '%s' "$ZOT_PASSWORD" | bao kv put -mount=secret tap-ci/zot username=tap-ci password=- >/dev/null
bao kv put -mount=secret tap-ci/kube server="$K8S_HOST" ca=@/run/issuer/ca.crt >/dev/null
echo "    done" >&2
