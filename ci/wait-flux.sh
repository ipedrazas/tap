#!/usr/bin/env bash
# Wait until Flux has applied this commit's apps layer (deploy/flux/platform.yaml:
# Kustomization flux-system/platform) and everything it applied is ready. The
# artifact carries its origin, main@sha1:<commit>, so the Kustomization's
# lastAppliedOriginRevision says which commit is live. Same as tap-agents'
# ci/wait-flux.sh, for the platform Kustomization.
set -euo pipefail
. ci/lib.sh
want="main@sha1:$GITHUB_SHA"
# No nudge: the source polls every minute, and CI may not patch it (patch
# can't be limited to the reconcile annotation, so it would also reach the
# source's URL, tag and verify settings).
msg=""
for _ in $(seq 1 60); do
  got=$(kubectl -n flux-system get kustomization platform -o jsonpath='{.status.lastAppliedOriginRevision}')
  ready=$(kubectl -n flux-system get kustomization platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  msg=$(kubectl -n flux-system get kustomization platform -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')
  if [ "$got" = "$want" ] && [ "$ready" = True ]; then
    echo "applied and ready: $want"
    echo "### deployed: \`$want\`" >> "$GITHUB_STEP_SUMMARY"
    exit 0
  fi
  echo "waiting: applied ${got:-nothing} (ready: ${ready:-?}) ${msg}"
  sleep 10
done
echo "::error::Flux did not apply $want within 10 minutes: ${msg}"
exit 1
