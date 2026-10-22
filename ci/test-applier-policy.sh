#!/usr/bin/env bash
# test-applier-policy.sh: check deploy/flux/applier-policy.yaml in a throwaway
# kind cluster. As each Flux applier, what tapctl render and the apps layer
# emit must be admitted, and the escalations SC-1 describes must be denied.
# RBAC is wide open in the test cluster, so only the policies are tested.
#
# Needs docker, kind, kubectl and go. AGENTS_DIR=../tap-agents/agents also
# renders every agent there (with a stand-in bundle digest). KEEP=1 keeps the
# cluster.
set -euo pipefail
cd "$(dirname "$0")/.."

name=tap-applier-policy
dir=$(mktemp -d)
export KUBECONFIG=$dir/kubeconfig
# shellcheck disable=SC2329 # runs from the trap
cleanup() {
  [ -n "${KEEP:-}" ] || kind delete cluster --name "$name" >/dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT
kind create cluster --name "$name" --wait 120s --quiet

A=system:serviceaccount:flux-system:tap-agents-applier
P=system:serviceaccount:flux-system:tap-platform-applier

kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata: { name: flux-system }
---
apiVersion: v1
kind: Namespace
metadata: { name: tap-system }
---
apiVersion: v1
kind: Namespace
metadata: { name: tap-factory }
---
apiVersion: v1
kind: ServiceAccount
metadata: { name: tap-agents-applier, namespace: flux-system }
---
apiVersion: v1
kind: ServiceAccount
metadata: { name: tap-platform-applier, namespace: flux-system }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: appliers-admin }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin }
subjects:
  - { kind: ServiceAccount, name: tap-agents-applier, namespace: flux-system }
  - { kind: ServiceAccount, name: tap-platform-applier, namespace: flux-system }
EOF

# Schemaless stand-ins for the CRDs an agent uses.
for crd in "external-secrets.io v1 SecretStore secretstores" "external-secrets.io v1 ExternalSecret externalsecrets" \
  "external-secrets.io v1alpha1 PushSecret pushsecrets" "generators.external-secrets.io v1alpha1 Password passwords" \
  "gateway.networking.k8s.io v1 HTTPRoute httproutes"; do
  # shellcheck disable=SC2086 # split into its four fields
  set -- $crd
  cat <<EOF
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: $4.$1
  annotations: { api-approved.kubernetes.io: "https://github.com/kubernetes-sigs/gateway-api/pull/1" }
spec:
  group: $1
  scope: Namespaced
  names: { kind: $3, plural: $4, singular: $(echo "$3" | tr '[:upper:]' '[:lower:]'), listKind: ${3}List }
  versions:
    - name: $2
      served: true
      storage: true
      schema: { openAPIV3Schema: { type: object, x-kubernetes-preserve-unknown-fields: true } }
EOF
done | kubectl apply -f - >/dev/null
kubectl wait --for=condition=Established crd --all --timeout=60s >/dev/null
kubectl apply -f deploy/flux/applier-policy.yaml >/dev/null
# Policies take a moment to reach the admission plugin.
for _ in $(seq 30); do
  kubectl --as=$A create namespace probe-not-agent --dry-run=server >/dev/null 2>&1 || break
  sleep 1
done

failed=0
ok() { echo "ok    $*"; }
bad() { echo "FAIL  $*"; failed=1; }

# admit WHO LABEL FILE: applied for real (later cases build on it).
admit() {
  if out=$(kubectl --as="$1" apply -f "$3" 2>&1); then ok "admitted: $2"; else bad "admitted: $2"; echo "$out" | grep -v Warning; fi
}
# deny WHO LABEL: the manifest on stdin is refused by a policy.
deny() {
  local f=$dir/case.yaml out
  cat > "$f"
  out=$(kubectl --as="$1" apply --dry-run=server -f "$f" 2>&1) || true
  case $out in *"denied request"*) ok "denied: $2" ;; *) bad "denied: $2"; echo "$out" | grep -v Warning ;; esac
}
deployment() { # NS NAME SA [container fields] [pod fields]
  cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: $2, namespace: $1 }
spec:
  selector: { matchLabels: { app: x } }
  template:
    metadata: { labels: { app: x } }
    spec:
      serviceAccountName: $3
      containers: [{ name: c, image: busybox${4:-} }]
${5:-}
EOF
}
namespace() { # NAME LABELS
  printf 'apiVersion: v1\nkind: Namespace\nmetadata: { name: %s, labels: { %s } }\n' "$1" "$2"
}

for f in pkg/render/testdata/*.yaml; do admit $A "render ${f##*/}" "$f"; done
if [ -n "${AGENTS_DIR:-}" ]; then
  go build -o "$dir/tapctl" ./cmd/tapctl
  for a in "$AGENTS_DIR"/*/; do
    n=$(basename "$a")
    "$dir/tapctl" render --platform platform.yaml --bundle "registry.hiddenfield.dev/agents/$n@sha256:$(printf '0%.0s' $(seq 64))" "$a" > "$dir/$n.yaml"
    admit $A "render $n" "$dir/$n.yaml"
  done
fi
for k in deploy/platform/apps deploy/factory/apps; do
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$k" > "$dir/apps.yaml"
  admit $P "apps layer $k" "$dir/apps.yaml"
done
# Fresh names, so the cases aren't rejected for changing a Deployment's selector.
agentns=agent-echo-agent
kubectl -n $agentns delete deployment agent >/dev/null
kubectl -n tap-system delete deployment tap-console >/dev/null
kubectl -n tap-factory delete deployment factory >/dev/null

deployment flux-system evil kustomize-controller | deny $A "agents: Deployment in flux-system as kustomize-controller"
deployment $agentns agent tap-secrets | deny $A "agents: agent Deployment as another service account"
deployment $agentns agent agent "" "      volumes: [{ name: h, hostPath: { path: / } }]" | deny $A "agents: hostPath"
deployment $agentns agent agent "" "      hostNetwork: true" | deny $A "agents: hostNetwork"
deployment $agentns agent agent ", securityContext: { privileged: true }" | deny $A "agents: privileged container"
namespace evil 'tap.hiddenfield.dev/agent: evil' | deny $A "agents: namespace outside agent-*"
namespace agent-x 'tap.hiddenfield.dev/agent: "y", tap.hiddenfield.dev/admission: verify, pod-security.kubernetes.io/enforce: baseline' | deny $A "agents: namespace labelled for another agent"
namespace agent-x 'tap.hiddenfield.dev/agent: x, pod-security.kubernetes.io/enforce: baseline' | deny $A "agents: namespace Kyverno doesn't see"
namespace agent-x 'tap.hiddenfield.dev/agent: x, tap.hiddenfield.dev/admission: verify, tap.hiddenfield.dev/preview: "true", pod-security.kubernetes.io/enforce: baseline' | deny $A "agents: namespace labelled preview"
namespace agent-x 'tap.hiddenfield.dev/agent: x, tap.hiddenfield.dev/admission: verify, pod-security.kubernetes.io/enforce: privileged' | deny $A "agents: privileged pod security"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: { name: zot, namespace: tap-system }\n' | deny $A "agents: other ConfigMaps in tap-system"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: { name: egress-policy-echo-agent, namespace: tap-system, labels: { tap.hiddenfield.dev/egress-policy: tap-factory } }\n' | deny $A "agents: egress policy labelled for another agent"
printf 'apiVersion: gateway.networking.k8s.io/v1\nkind: HTTPRoute\nmetadata: { name: agent, namespace: %s }\nspec: { parentRefs: [{ name: tap, namespace: tap-system, sectionName: agents }], hostnames: [weather-agent.a.hiddenfield.dev] }\n' $agentns | deny $A "agents: another agent's hostname"
printf 'apiVersion: external-secrets.io/v1\nkind: SecretStore\nmetadata: { name: openbao, namespace: %s }\nspec: { provider: { vault: { auth: { kubernetes: { serviceAccountRef: { name: agent } } } } } }\n' $agentns | deny $A "agents: SecretStore as another service account"
printf 'apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: { name: evil }\nroleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin }\nsubjects: [{ kind: ServiceAccount, name: agent, namespace: %s }]\n' $agentns | deny $A "agents: cluster-scoped objects"

deployment tap-system tap-console tap-platform-secrets | deny $P "platform: Deployment as another service account"
deployment tap-system evil evil | deny $P "platform: Deployment outside the apps layer"
deployment flux-system kustomize-controller kustomize-controller | deny $P "platform: Deployment in flux-system"
deployment tap-system tap-console tap-console "" "      volumes: [{ name: s, secret: { secretName: zot-htpasswd } }]" | deny $P "platform: another Secret as a volume"
deployment tap-system tap-console tap-console "" "      volumes: [{ name: s, projected: { sources: [{ secret: { name: zot-htpasswd } }] } }]" | deny $P "platform: another Secret, projected"
deployment tap-factory factory factory ", envFrom: [{ secretRef: { name: github-token } }]" | deny $P "platform: Secret through envFrom"
deployment tap-factory factory factory ", env: [{ name: T, valueFrom: { secretKeyRef: { name: github-token, key: token } } }]" | deny $P "platform: Secret through env"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: { name: zot, namespace: tap-system }\n' | deny $P "platform: the zot ConfigMap"
printf 'apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: { name: default-deny-ingress, namespace: tap-system }\nspec: { podSelector: {} }\n' | deny $P "platform: the infra layer's NetworkPolicy"

exit $failed
