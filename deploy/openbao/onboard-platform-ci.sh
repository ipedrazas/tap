#!/bin/sh
#
# Let CI in this repo build, push and sign the platform service images without
# stored credentials. Idempotent. Run it with `task ci:onboard:platform`, which
# starts this script in a pod in tap-system and passes the admin token and the
# zot password of tap-platform-ci on stdin (one per line). It sets up:
#   - auth/jwt-github (shared with the agents repo's CI, see onboard-ci.sh)
#   - role tap-platform-ci: GH_REPO's WORKFLOW on main; policies tap-signer,
#     tap-platform-ci
#   - secret/tap-platform-ci/zot (CI push user)
#
# Inputs: BAO_ADDR, GH_REPO, WORKFLOW (env); admin token and zot password on
# stdin; policies in /policies.
set -eu

: "${BAO_ADDR:?}" "${GH_REPO:?}" "${WORKFLOW:?}"
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

echo "==> CI settings at secret/tap-platform-ci/" >&2
printf '%s' "$ZOT_PASSWORD" | bao kv put -mount=secret tap-platform-ci/zot username=tap-platform-ci password=- >/dev/null
echo "    done" >&2
