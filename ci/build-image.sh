#!/usr/bin/env bash
# build-image.sh IMAGE: push one image of the apps layer under its input tag
# (deploy/apps.sh tag) and sign it with tap-build. A tag that's already in the
# registry is never rebuilt: the same sources would get a new digest. Same
# build as `task images:push`.
set -euo pipefail
. ci/lib.sh

image=$1
tag=$(deploy/apps.sh tag "$image")
case $tag in *-dirty) echo "::error::$image: the checkout changes its inputs"; exit 1 ;; esac
ref="$REGISTRY/tap/$image:$tag"

if crane digest "$ref" >/dev/null 2>&1; then
  echo "$ref is already pushed"
  built=no
else
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
    -f "images/$image/Dockerfile" \
    --build-arg VERSION="$(git describe --tags --always)" \
    --build-arg COMMIT="$(git rev-parse HEAD)" \
    --build-arg DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --output "type=image,name=$ref,push=true,oci-mediatypes=true" .
  built=yes
fi
digest=$(crane digest "$ref")
sign_once "$REGISTRY/tap/$image@$digest"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "| $image | \`$tag\` | $built | \`$digest\` |" >> "$GITHUB_STEP_SUMMARY"
