#!/bin/sh
#
# Secret isolation checks (task secrets:check). Stdin: three service-account
# tokens for AGENT_NS, one per line: tap-secrets with audience "openbao",
# tap-secrets with another audience, and the pod account "agent". Env: OWN and
# OTHER are KV paths of a secret of this agent and of another agent; NS and
# OTHER_NS their namespaces. Prints PASS/FAIL per check; values are never
# printed.
set -u
: "${OWN:?}" "${OTHER:?}" "${NS:?}" "${OTHER_NS:?}"
read -r good; read -r wrong_aud; read -r pod_sa
login() { bao write -field=token auth/kubernetes-tap/login role=tap-agent jwt="$1" 2>/dev/null; }
failed=0
check() { if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1 (got $2, want $3)"; failed=1; fi; }
readable() { BAO_TOKEN=$1 bao kv get -mount=secret -field=value "$2" >/dev/null 2>&1 && echo readable || echo denied; }
# What the policy grants on a KV v2 data path, whether or not it exists.
caps() { BAO_TOKEN=$1 bao token capabilities "secret/data/$2" 2>/dev/null | tr -d ' []' ; }
can() { case ",$(caps "$1" "$2")," in *",$3,"*) echo yes ;; *) echo no ;; esac; }

t=$(login "$good")
case $t in s.*) check "tap-secrets logs in" ok ok ;; *) check "tap-secrets logs in" failed ok; exit 1 ;; esac
check "reads its own secret" "$(readable "$t" "$OWN")" readable
check "cannot read another agent's secret" "$(readable "$t" "$OTHER")" denied
check "cannot read outside tap/" "$(readable "$t" "tap-isolation-probe/x")" denied
check "reads the shared model key" "$(can "$t" tap-platform/model-credentials read)" yes
check "cannot write the shared model key" "$(can "$t" tap-platform/model-credentials update)" no
check "cannot read other platform secrets" "$(caps "$t" tap-platform/sessions-reader)" deny
check "writes its own egress key" "$(can "$t" "tap-egress/$NS" create)" yes
check "cannot touch another agent's egress key" "$(caps "$t" "tap-egress/$OTHER_NS")" deny
case $(login "$wrong_aud") in s.*) r=accepted ;; *) r=rejected ;; esac
check "token with another audience is rejected" "$r" rejected
case $(login "$pod_sa") in s.*) r=accepted ;; *) r=rejected ;; esac
check "the pod's service account is rejected" "$r" rejected
exit $failed
