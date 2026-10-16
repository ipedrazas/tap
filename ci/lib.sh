# shellcheck shell=bash
# Sourced by the platform workflow's steps. Credentials come from OpenBao,
# which trusts GitHub's OIDC token (auth/jwt-github, role tap-platform-ci, set
# up by `task ci:onboard:platform`): nothing is stored in GitHub. The same
# helpers as tap-agents' ci/lib.sh, for this repo's role and zot user.
#
# Env: VAULT_ADDR, REGISTRY; ACTIONS_ID_TOKEN_REQUEST_{URL,TOKEN} (id-token: write).
set -eu

# bao_login <role>: exchange the job's OIDC token for an OpenBao token and
# export it (masked) to this and later steps as VAULT_TOKEN.
bao_login() {
  jwt=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=openbao" | jq -r .value)
  VAULT_TOKEN=$(jq -n --arg role "$1" --arg jwt "$jwt" '{role: $role, jwt: $jwt}' \
    | curl -sSf -X POST --data @- "$VAULT_ADDR/v1/auth/jwt-github/login" | jq -r .auth.client_token)
  [ -n "$VAULT_TOKEN" ] && [ "$VAULT_TOKEN" != null ] || { echo "::error::OpenBao login as $1 failed"; return 1; }
  echo "::add-mask::$VAULT_TOKEN"
  export VAULT_TOKEN
  [ -z "${GITHUB_ENV:-}" ] || echo "VAULT_TOKEN=$VAULT_TOKEN" >> "$GITHUB_ENV"
}

bao_get() { # <path under secret/> <field>
  curl -sSf -H "X-Vault-Token: $VAULT_TOKEN" "$VAULT_ADDR/v1/secret/data/$1" | jq -r ".data.data[\"$2\"]"
}

# registry_login: the tap-platform-ci push user, for docker, crane and cosign
# (all read the Docker config).
registry_login() {
  pw=$(bao_get tap-platform-ci/zot password)
  echo "::add-mask::$pw"
  printf '%s' "$pw" | docker login "$REGISTRY" -u "$(bao_get tap-platform-ci/zot username)" --password-stdin >/dev/null
}

# pubkey: the build key admission and Flux trust, from platform.yaml.
pubkey() {
  mkdir -p .tap
  go run ./cmd/tapctl signing key build > .tap/cosign.pub
}

verified() { # <ref>
  cosign verify --key .tap/cosign.pub --insecure-ignore-tlog=true "$1" >/dev/null 2>&1
}

# sign_once <ref>: sign with tap-build unless a valid signature exists (ECDSA
# signatures differ every time, so re-signing would pile them up).
sign_once() {
  verified "$1" && { echo "already signed: $1"; return 0; }
  cosign sign --key hashivault://tap-build --tlog-upload=false --yes "$1" 2>&1 | grep -v '^WARNING' || true
  verified "$1" || { echo "::error::signing $1 failed"; return 1; }
  echo "signed: $1"
}
