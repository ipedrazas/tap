#!/usr/bin/env bash
# Render the apps layer at its images' input tags (deploy/apps.sh render) and
# push it as one signed OCI artifact, registry/tap/platform-manifests:<commit>
# tagged main, which Flux will apply after verifying the signature
# (docs/platform-gitops-plan.md, phase 4). Fails if an image isn't signed with
# tap-build or the render names one without a digest.
set -euo pipefail
. ci/lib.sh
pubkey

for ref in $(deploy/apps.sh refs); do
  verified "$ref" || { echo "::error::$ref is not signed with tap-build"; exit 1; }
  echo "- \`$ref\`" >> "$GITHUB_STEP_SUMMARY"
done

out=.tap/platform-manifests
rm -rf "$out"; mkdir -p "$out"
deploy/apps.sh render > "$out/apps.yaml"
if unpinned=$(grep -E '^\s*image: ' "$out/apps.yaml" | grep -v '@sha256:[a-f0-9]\{64\}$'); then
  echo "::error::images without a digest: $unpinned"; exit 1
fi

repo="$REGISTRY/tap/platform-manifests"
digest=$(flux push artifact "oci://$repo:$GITHUB_SHA" --path="$out" \
  --source="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY" --revision="main@sha1:$GITHUB_SHA" \
  --output json | jq -r .digest)
case "$digest" in sha256:*) ;; *) echo "::error::flux push artifact gave no digest"; exit 1 ;; esac
sign_once "$repo@$digest"
flux tag artifact "oci://$repo:$GITHUB_SHA" --tag main
echo "### manifests: \`$repo@$digest\` (tags $GITHUB_SHA, main)" >> "$GITHUB_STEP_SUMMARY"
