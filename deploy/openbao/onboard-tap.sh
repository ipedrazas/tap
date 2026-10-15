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
#   - role tap-console: service account tap-console in tap-system, audience
#     "openbao", policy tap-console (the console sets secret values)
#   - role tap-platform: service account tap-platform-secrets in tap-system,
#     policy tap-platform (ESO pushes the platform secrets agents share)
#   - role tap-egress-proxy: service account egress-proxy in tap-system,
#     policy tap-egress-proxy (reads agent egress keys)
#   - Transit at tap-transit/ with the signing keys tap-build and tap-tested
#     (ECDSA P-256, not exportable, not deletable): cosign signs through
#     hashivault://, and the private keys never leave OpenBao
#   - policies tap-seeder (write agent secrets), tap-signer and
#     tap-tested-signer (sign with one key each), and a periodic operator
#     token holding all three, printed last
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

echo "==> Transit at tap-transit/ (signing keys)" >&2
bao secrets enable -path=tap-transit transit >/dev/null 2>&1 && echo "    enabled" >&2 || echo "    already enabled" >&2
for key in tap-build tap-tested; do
  if bao read tap-transit/keys/$key >/dev/null 2>&1; then
    echo "    key $key exists (kept)" >&2
  else
    bao write -f tap-transit/keys/$key type=ecdsa-p256 exportable=false allow_plaintext_backup=false >/dev/null
    echo "    key $key created" >&2
  fi
done

echo "==> Policies tap-agent, tap-seeder, tap-console, tap-platform, tap-egress-proxy, tap-signer, tap-tested-signer" >&2
sed "s/ACCESSOR/$accessor/" /policies/tap-agent-policy.hcl | bao policy write tap-agent - >/dev/null
bao policy write tap-seeder /policies/tap-seeder-policy.hcl >/dev/null
bao policy write tap-console /policies/tap-console-policy.hcl >/dev/null
bao policy write tap-platform /policies/tap-platform-policy.hcl >/dev/null
bao policy write tap-egress-proxy /policies/tap-egress-proxy-policy.hcl >/dev/null
bao policy write tap-signer /policies/tap-signer-policy.hcl >/dev/null
bao policy write tap-tested-signer /policies/tap-tested-signer-policy.hcl >/dev/null

echo "==> Role tap-agent" >&2
bao write auth/kubernetes-tap/role/tap-agent \
  bound_service_account_names=tap-secrets \
  bound_service_account_namespaces='*' \
  audience=openbao \
  token_policies=tap-agent \
  token_ttl=10m token_max_ttl=30m >/dev/null

echo "==> Role tap-console" >&2
bao write auth/kubernetes-tap/role/tap-console \
  bound_service_account_names=tap-console \
  bound_service_account_namespaces=tap-system \
  audience=openbao \
  token_policies=tap-console \
  token_ttl=10m token_max_ttl=30m >/dev/null

echo "==> Role tap-platform" >&2
bao write auth/kubernetes-tap/role/tap-platform \
  bound_service_account_names=tap-platform-secrets \
  bound_service_account_namespaces=tap-system \
  audience=openbao \
  token_policies=tap-platform \
  token_ttl=10m token_max_ttl=30m >/dev/null

echo "==> Role tap-egress-proxy" >&2
bao write auth/kubernetes-tap/role/tap-egress-proxy \
  bound_service_account_names=egress-proxy \
  bound_service_account_namespaces=tap-system \
  audience=openbao \
  token_policies=tap-egress-proxy \
  token_ttl=10m token_max_ttl=30m >/dev/null

echo "==> Operator token (orphan, periodic $SEEDER_PERIOD; renewed on every use)" >&2
bao token create -orphan -policy=tap-seeder -policy=tap-signer -policy=tap-tested-signer \
  -period="$SEEDER_PERIOD" -display-name=tap-operator -field=token
