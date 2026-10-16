#!/bin/sh
#
# Let CI in this repo build, push and sign the platform service images without
# stored credentials. Idempotent. Run it with `task ci:onboard:platform`, which
# starts this script in a pod in tap-system and passes the admin token and the
# zot password of tap-platform-ci on stdin (one per line). It sets up:
#   - auth/jwt-github (shared with the agents repo's CI, see onboard-ci.sh)
#   - role tap-platform-ci: GH_REPO's WORKFLOW on main; policies tap-signer,
#     tap-platform-ci
#   - tap-kubernetes/roles/tap-platform-ci-runner: short-lived tokens for
#     flux-system/tap-platform-ci-runner (deploy/ci/platform-rbac.yaml). The
#     engine itself is configured by onboard-ci.sh (task ci:onboard).
#   - secret/tap-platform-ci/zot (CI push user) and secret/tap-platform-ci/kube
#     (API server, CA)
#
# Inputs: BAO_ADDR, K8S_HOST, GH_REPO, WORKFLOW (env); admin token and zot
# password on stdin; the cluster CA at /run/issuer/ca.crt; policies in
# /policies.
set -eu

: "${BAO_ADDR:?}" "${K8S_HOST:?}" "${GH_REPO:?}" "${WORKFLOW:?}"
read -r BAO_TOKEN
read -r ZOT_PASSWORD
export BAO_ADDR BAO_TOKEN
[ -n "$ZOT_PASSWORD" ] || { echo "ERROR: no zot password on stdin" >&2; exit 1; }

bao token lookup >/dev/null || { echo "ERROR: the token was rejected" >&2; exit 1; }

echo "==> JWT auth at auth/jwt-github (GitHub Actions OIDC)" >&2
bao auth enable -path=jwt-github jwt >/dev/null 2>&1 && echo "    enabled" >&2 || echo "    already enabled" >&2
bao write auth/jwt-github/config \
  oidc_discovery_url=https://token.actions.githubusercontent.com \
  bound_issuer=https://token.actions.githubusercontent.com >/dev/null

echo "==> Policy tap-platform-ci" >&2
bao policy write tap-platform-ci /policies/tap-platform-ci-policy.hcl >/dev/null

workflow_ref="$GH_REPO/.github/workflows/$WORKFLOW@refs/heads/main"
echo "==> Role tap-platform-ci for $workflow_ref" >&2
cat <<JSON | bao write auth/jwt-github/role/tap-platform-ci - >/dev/null
{
  "role_type": "jwt",
  "user_claim": "sub",
  "bound_audiences": ["openbao"],
  "bound_claims_type": "string",
  "bound_claims": {
    "repository": "$GH_REPO",
    "ref": "refs/heads/main",
    "job_workflow_ref": "$workflow_ref"
  },
  "token_policies": "tap-signer,tap-platform-ci",
  "token_ttl": "30m",
  "token_max_ttl": "1h"
}
JSON

echo "==> Role tap-platform-ci-runner at tap-kubernetes/" >&2
bao read tap-kubernetes/config >/dev/null 2>&1 || { echo "ERROR: tap-kubernetes/ isn't configured; run: task ci:onboard" >&2; exit 1; }
bao write tap-kubernetes/roles/tap-platform-ci-runner \
  service_account_name=tap-platform-ci-runner \
  allowed_kubernetes_namespaces=flux-system \
  token_default_ttl=30m token_max_ttl=1h >/dev/null

echo "==> CI settings at secret/tap-platform-ci/" >&2
printf '%s' "$ZOT_PASSWORD" | bao kv put -mount=secret tap-platform-ci/zot username=tap-platform-ci password=- >/dev/null
bao kv put -mount=secret tap-platform-ci/kube server="$K8S_HOST" ca=@/run/issuer/ca.crt >/dev/null
echo "    done" >&2
