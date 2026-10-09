#!/bin/sh
#
# Runs a bao script in a short-lived pod in tap-system, so tap talks to
# OpenBao from inside the cluster, over TLS, whatever the operator's network.
# Stdin is passed through (tokens and secret values never appear in args or
# the pod spec).
#
#   deploy/openbao/run.sh <script> [--reviewer] [KEY=value ...]
#
# --reviewer mounts the openbao-reviewer token and the policies (onboarding).
set -eu
script=$1; shift
reviewer=false
envs="[]"
for a in "$@"; do
  case $a in
    --reviewer) reviewer=true ;;
    *=*) envs=$(python3 -c 'import json,sys; e=json.loads(sys.argv[1]); k,v=sys.argv[2].split("=",1); e.append({"name":k,"value":v}); print(json.dumps(e))' "$envs" "$a") ;;
  esac
done
name="bao-$(date +%s)-$$"
if $reviewer; then
  kubectl -n tap-system create configmap "$name-policies" --from-file=deploy/openbao/tap-agent-policy.hcl --from-file=deploy/openbao/tap-seeder-policy.hcl >/dev/null
  trap 'kubectl -n tap-system delete configmap "$name-policies" --ignore-not-found >/dev/null' EXIT
fi
overrides=$(python3 - "$name" "$reviewer" "$envs" "$(cat "$script")" <<'PY'
import json, sys
name, reviewer, envs, script = sys.argv[1], sys.argv[2] == "true", json.loads(sys.argv[3]), sys.argv[4]
c = {
    "name": "bao", "image": "openbao/openbao:2.5.5",
    "command": ["sh", "-c", script],
    "stdin": True, "stdinOnce": True,
    "env": envs + [{"name": "BAO_ADDR", "value": "https://openbao.alacasa.uk:8443"}, {"name": "HOME", "value": "/tmp"}],
    "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
    "volumeMounts": [{"name": "tmp", "mountPath": "/tmp"}],
}
vols = [{"name": "tmp", "emptyDir": {"medium": "Memory", "sizeLimit": "8Mi"}}]
if reviewer:
    c["volumeMounts"] += [{"name": "reviewer", "mountPath": "/run/reviewer", "readOnly": True},
                          {"name": "policies", "mountPath": "/policies", "readOnly": True}]
    vols += [{"name": "reviewer", "secret": {"secretName": "openbao-reviewer-token"}},
             {"name": "policies", "configMap": {"name": name + "-policies"}}]
print(json.dumps({"spec": {
    "automountServiceAccountToken": False, "enableServiceLinks": False,
    "securityContext": {"runAsNonRoot": True, "runAsUser": 100, "runAsGroup": 1000, "seccompProfile": {"type": "RuntimeDefault"}},
    "containers": [c], "volumes": vols}}))
PY
)
kubectl -n tap-system run "$name" -i --rm --quiet --restart=Never --image=openbao/openbao:2.5.5 \
  --overrides="$overrides" --pod-running-timeout=2m
