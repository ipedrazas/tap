#!/usr/bin/env bash
# build-image.sh IMAGE: push one image of the apps layer under its input tag
# (deploy/apps.sh tag) and sign it with tap-build. A tag that's already in the
# registry is never rebuilt: the same sources would get a new digest. It is
# only accepted if CI signed it when it built it: this job signs only what it
# built, never whatever someone else pushed under a tag it can predict.
set -euo pipefail
. ci/lib.sh

image=$1
tag=$(deploy/apps.sh tag "$image")
case $tag in *-dirty) echo "::error::$image: the checkout changes its inputs"; exit 1 ;; esac
ref="$REGISTRY/tap/$image:$tag"

if digest=$(crane digest "$ref" 2>/dev/null); then
  echo "$ref is already pushed"
  verified "$REGISTRY/tap/$image@$digest" || {
    echo "::error::$ref is in the registry without a tap-build signature, so CI didn't build it. Find out who pushed it; to rebuild, delete the tag (crane delete $REGISTRY/tap/$image@$digest) and rerun."
    exit 1
  }
  built=no
else
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
    -f "images/$image/Dockerfile" \
    --build-arg VERSION="$(git describe --tags --always)" \
    --build-arg COMMIT="$(git rev-parse HEAD)" \
    --build-arg DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --output "type=image,name=$ref,push=true,oci-mediatypes=true" .
  built=yes
  digest=$(crane digest "$ref")
  sign_once "$REGISTRY/tap/$image@$digest"
fi
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "| $image | \`$tag\` | $built | \`$digest\` |" >> "$GITHUB_STEP_SUMMARY"
