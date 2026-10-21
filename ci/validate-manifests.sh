#!/usr/bin/env bash
# validate-manifests.sh: check every manifest this repo applies or renders
# against the Kubernetes and CRD schemas (kubeconform), without a cluster:
# the kustomize layers, Flux and CI objects, the admission policies, and
# tapctl render's output (pkg/render/testdata, which the render tests keep
# current). Catches a broken render or
# an invalid label before merge instead of at Flux's next apply.
#
# Needs kubectl (kustomize), kubeconform and go.
set -euo pipefail
cd "$(dirname "$0")/.."

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

for k in deploy/platform/infra deploy/platform/apps deploy/factory/infra deploy/factory/apps; do
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$k" > "$out/$(echo "$k" | tr / -).yaml"
done
cp deploy/flux/*.yaml deploy/ci/*.yaml "$out/"

go run ./cmd/tapctl admission policy > "$out/admission.yaml"
# tapctl render's output, as the golden render tests pin it.
for f in pkg/render/testdata/*.yaml; do cp "$f" "$out/render-${f##*/}"; done

# CRDs (Gateway API, Envoy Gateway, External Secrets, Kyverno, Flux) come
# from the community catalog; a kind it doesn't have is reported as skipped.
kubeconform -strict -summary -output text \
  -kubernetes-version 1.36.0 \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
  -ignore-missing-schemas \
  "$out"/*.yaml
