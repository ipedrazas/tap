#!/bin/sh
#
# Onboard tap into the central OpenBao. Idempotent; every run mints a fresh
# seeder token (older ones stay valid until they expire or are revoked).
#
# Run it with `task openbao:onboard`, which starts this script in a pod in
# tap-system (talking to OpenBao over TLS on 8443) and passes the admin token
# on stdin. It sets up:
#   - KV v2 at secret/ (shared with the other apps; tap uses secret/tap/)
#   - auth/kubernetes-tap: Kubernetes auth against the k3s API, using the
#     openbao-reviewer service account token for TokenReview
#   - role tap-agent: service account tap-secrets in any namespace, audience
#     "openbao", policy tap-agent (each namespace reads only its own path)
#   - policy tap-seeder and a periodic token for writing secrets, printed last
#
# Inputs: BAO_ADDR, K8S_HOST (env); the admin token on stdin; the reviewer
# token and cluster CA at /run/reviewer/{token,ca.crt}; policies in /policies.
set -eu

: "${BAO_ADDR:?}" "${K8S_HOST:?}"
read -r BAO_TOKEN
export BAO_ADDR BAO_TOKEN
SEEDER_PERIOD="${SEEDER_PERIOD:-768h}"

bao token lookup >/dev/null || { echo "ERROR: the token was rejected" >&2; exit 1; }

echo "==> KV v2 at secret/" >&2
bao secrets enable -path=secret -version=2 kv >/dev/null 2>&1 && echo "    enabled" >&2 || echo "    already enabled" >&2

echo "==> Kubernetes auth at auth/kubernetes-tap" >&2
bao auth enable -path=kubernetes-tap kubernetes >/dev/null 2>&1 && echo "    enabled" >&2 || echo "    already enabled" >&2
bao write auth/kubernetes-tap/config \
  kubernetes_host="$K8S_HOST" \
  kubernetes_ca_cert=@/run/reviewer/ca.crt \
  token_reviewer_jwt=@/run/reviewer/token \
  disable_local_ca_jwt=true >/dev/null
accessor=$(bao read -field=accessor sys/auth/kubernetes-tap)
echo "    accessor $accessor" >&2

echo "==> Policies tap-agent, tap-seeder" >&2
sed "s/ACCESSOR/$accessor/" /policies/tap-agent-policy.hcl | bao policy write tap-agent - >/dev/null
bao policy write tap-seeder /policies/tap-seeder-policy.hcl >/dev/null

echo "==> Role tap-agent" >&2
bao write auth/kubernetes-tap/role/tap-agent \
  bound_service_account_names=tap-secrets \
  bound_service_account_namespaces='*' \
  audience=openbao \
  token_policies=tap-agent \
  token_ttl=10m token_max_ttl=30m >/dev/null

echo "==> Seeder token (orphan, periodic $SEEDER_PERIOD; renewed on every use)" >&2
bao token create -orphan -policy=tap-seeder -period="$SEEDER_PERIOD" \
  -display-name=tap-seeder -field=token
