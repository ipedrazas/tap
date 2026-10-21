# tap: Runbook

Routine operations and known failures. How the pieces fit is in [architecture.md](architecture.md); every task is listed by `task --list`.

## Before you start

```bash
task tools tapctl                          # oras, crane, cosign; bin/tapctl
kubectl config current-context             # the k3s cluster
deploy/openbao/token.sh >/dev/null && echo ok   # operator token in the Keychain
task registry:login                        # checks the push password first
```

Never `kubectl apply -f` a single file from `deploy/platform` or `deploy/factory`: the namespace comes from the kustomization, so a bare apply lands in `default`. Use `task platform:up` (the infra layer) or pass `-n tap-system`. The services (the apps layer) are applied by Flux from main; `task platform:apps FORCE=1` is the break-glass path.

## Routine operations

### Ship a change to a platform service

Rebuild only what changed. A full `task images:push` also rebuilds the runners, and new runner digests make every agent fail validation until it is re-tested.

```bash
task images:push IMAGES="harness hooks console"   # builds, signs, pins digests
git diff platform.yaml deploy/                     # commit the new digests
task platform:up                                   # tap-system services
task agent:deploy AGENT=<name>                     # each agent, if the harness changed
```

Push and pin a new harness before redeploying agents whose rendered manifests depend on it (new flags, ports): an old harness exits on flags it doesn't know.

### Ship an agent

In tap-agents:

```bash
task agent:validate AGENT=<name>
task agent:test:local AGENT=<name>   # fast loop on your machine
pbpaste | task secrets:put AGENT=<name> NAME=<NAME>   # before merging, for each declared secret
```

Open a PR and merge it; nothing else.
1. The publish workflow builds, pushes and signs every agent whose sources changed (bump `metadata.version`).
2. It runs their fixtures in `tap-ci`, then attests and signs them as tested.
3. It publishes `tap/agents-manifests` (signed).
4. It waits for Flux and sets the commit status `tap/deploy`.

A missing secret shows up as a failed `tap/deploy` (the ExternalSecret isn't Ready). Store it, then re-run the deploy job. Rolling back is reverting the commit in tap-agents.

`agent:deploy` (and `agent:launch`, `agent:dev`) refuse agents that Flux manages. `FORCE=1` deploys anyway, and Flux reverts it at its next reconcile.

### Previews

Try a PR's agent in the cluster before merging. In tap-agents, on the PR's branch:

```bash
pbpaste | task secrets:put AGENT=<name> NAME=<NAME> PREVIEW=1   # the preview's own secrets, if it declares any
task agent:preview AGENT=<name>      # fixtures in tap-ci, deploy <name>-dev, smoke chat
task agent:preview:rm AGENT=<name>   # when done
```

The preview runs at `https://<name>-dev.a.hiddenfield.dev`, signed as built but not tested; admission policy `tap-previews` allows that in preview namespaces only. `tapctl agent ls --previews` lists running previews; `tapctl agent ls` names any older than seven days, with the command that removes each.

### Flux

`task flux:status` shows what Flux last fetched and applied (`main@sha1:<tap-agents commit>`).
- **Apply now:** `kubectl -n flux-system annotate ocirepository agents reconcile.fluxcd.io/requestedAt=$(date +%s) --overwrite`.
- **Pause:** `kubectl -n flux-system patch kustomization agents --type merge -p '{"spec":{"suspend":true}}'`; resume with `false`.

Flux only fetches an artifact signed with `platform.yaml`'s build key (`SourceVerified`). After `task signing:pin`, rerun `task flux:install` to update `flux-system/tap-build-pub`.

To upgrade Flux, download the release's `install.yaml` and trim it with `yq --from-file deploy/flux/trim.yq install.yaml`. Then pin the three controller images by digest (`crane digest`), replace `deploy/flux/flux.yaml`, and run `task flux:install`.

### Re-pin the runners

Only when the runner itself changed. `tapctl runner bump agents/<name>` per agent in tap-agents (after a release with the new digests and a `tap.version` bump), bump `metadata.version`, then `task agent:test` and `task agent:deploy` for each. If a rebuild produced new runner digests by accident, pin the previous ones back instead (`bin/tapctl platform pin runner-node <ref@sha256:…>`) after checking they verify: `cosign verify --key .tap/cosign.pub --insecure-ignore-tlog <ref>`.

### Agent secrets

```bash
pbpaste | task secrets:put AGENT=<name> NAME=GITHUB_TOKEN   # or the console's Secrets card
task secrets:check                                          # isolation checks
```

The agent picks a new value up within a minute or two without a restart.

### Hooks

Add, edit, rotate and delete hooks on the agent's console page. The secret is shown once; "New credential" replaces it. To test the public path:

```bash
curl -si -X POST https://hooks.hiddenfield.dev/h/<agent>/nope   # expect 404 {"error":"no such hook"}
kubectl -n tap-system logs deploy/tap-hooks -f                  # one line per delivery
```

Rotate the trigger token (tap-hooks ↔ harness):

```bash
kubectl -n tap-system delete secret hooks-trigger-token
task platform:up                                  # creates a new one
kubectl -n tap-system get pushsecret trigger-token # wait until it has pushed the new value to OpenBao (10 min at most)
kubectl -n tap-system rollout restart deploy/tap-hooks
# each agent: refetch the token now rather than within the hour, then restart
kubectl -n agent-<name> annotate externalsecret trigger-token force-sync=$(date +%s) --overwrite
kubectl -n agent-<name> rollout restart deploy/agent
```

The model key and the sessions write key work the same way: they're created in `tap-system` (`task model:client`, `task sessions:setup`), pushed to OpenBao `secret/tap-platform/` by `deploy/platform/openbao-platform.yaml`, and read by each agent's ExternalSecrets.

### Session traces

The console's run history (`https://tap.hiddenfield.dev/agents/<name>/runs`) lists every chat and hook run, newest first; open one to see each model reply and tool call, or download its JSON. It needs `tap-system/sessions-reader`: rerun `task sessions:setup` if the agent page has no Runs card. A run shows up after its first turn ends.

`task sessions:pull AGENT=<name>` downloads traces to `.tap/sessions/<name>/` for casa. Callers are stored as pseudonyms (`harness.UserKey`); hook runs have turn trigger `hook`.

### CI in tap-agents (one-time setup)

The publish workflow in tap-agents builds, tests, signs and publishes on every push to main, with no stored credentials. It needs:

1. **Tailscale** (admin console):
   - `tag:tap-ci` in `tagOwners`, and these grants:
     ```json
     {"src": ["tag:tap-ci"], "dst": ["192.168.2.225"], "ip": ["tcp:443"]},
     {"src": ["tag:tap-ci"], "dst": ["192.168.2.131"], "ip": ["tcp:8443"]},
     {"src": ["tag:tap-ci"], "dst": ["192.168.2.221"], "ip": ["tcp:6443"]}
     ```
   - A federated identity (Trust credentials → OpenID Connect): issuer GitHub Actions, subject `repo:ipedrazas@32796/tap-agents@1420334254:*`, scope `auth_keys` (write), tag `tag:tap-ci`. The repository uses GitHub's immutable subject format, so `sub` carries the owner and repository IDs (`gh api repos/ipedrazas/tap-agents/actions/oidc/customization/sub` prints the prefix). A pattern with the plain name never matches, and the token exchange fails with 403. OpenBao still binds signing to the main branch and the publish workflow (with the `repository`, `ref` and `job_workflow_ref` claims, which keep the plain format); the tailnet only gives network access to those three ports.
2. **GitHub** (tap-agents):
   ```bash
   gh variable set TS_CLIENT_ID -R ipedrazas/tap-agents --body <client id>
   gh variable set TS_AUDIENCE -R ipedrazas/tap-agents --body <audience>
   gh api -X PUT repos/ipedrazas/tap-agents/environments/tested --input - <<< '{"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}'
   gh api -X POST repos/ipedrazas/tap-agents/environments/tested/deployment-branch-policies -f name=main -f type=branch
   ```
3. **OpenBao, zot and the cluster:** `read -s BAO_TOKEN; export BAO_TOKEN; task ci:onboard` (admin token). This sets up GitHub OIDC login, the two signing roles, Kubernetes token minting for `tap-ci-runner`, and the zot `tap-ci` user, and restarts zot.

### Signing keys and the operator token

`task openbao:onboard` (with an admin token) creates the Transit keys if missing, the policies and a periodic operator token, which it stores in the Keychain. Every signing task renews it, so it only expires after 32 days unused; if it does, onboard again. If `task sign` says the Transit key is not `platform.yaml`'s signing key, run `task signing:pin`, commit, and `task admission:apply`.

## Troubleshooting

### Hook deliveries

What GitHub's Recent Deliveries (or the caller) sees:

| Status | Body | Meaning | Fix |
| --- | --- | --- | --- |
| 200 | `pong` | ping verified | none |
| 200 | `ignored` | event not in the hook's filter | add it to the hook's events, or untick it in the webhook |
| 200 | `duplicate` | same delivery ID or body within 24h | none; a redelivery of a run that started |
| 202 | `started` + `session_id` | run started | find it in traces or harness logs by session |
| 400 | `body must be JSON` | GitHub webhook content type is `application/x-www-form-urlencoded` | set it to `application/json` |
| 401 | signature error | wrong or missing secret | rotate the credential and paste it again |
| 404 | `no such hook` (JSON) | hook unknown or disabled | check the URL and the Enabled box |
| 404 | NPM "Dead Host" page (HTML) | the NPM proxy host is offline | fix its config (usually the Advanced tab) until it shows Online |
| 413 | `body too large` | over 1 MiB | none |
| 422 | `prompt template: …` | template failed on this payload | guard nested fields with `{{with .payload.x}}…{{end}}` |
| 429 | `too many deliveries` | over 30/min for the hook | wait, or split hooks |
| 502 | `the agent is unavailable` | trigger call failed | agent down, or not redeployed since hooks were enabled (no `:8081`); check tap-hooks logs |
| 503 | `agent is busy` | two hook runs already in flight | GitHub doesn't retry; redeliver later |
| 503 | `hooks are unavailable` | tap-hooks can't read its settings | check its Role and the API server |

Path checks, from the outside in: Cloudflare DNS → NPM proxy host Online → `curl http://<tailnet ip>/healthz` from the VPS → `kubectl -n tailscale get pods` → tap-hooks logs. The tailnet IP is in the operator's state secret (`kubectl -n tailscale get secret -l tailscale.com/parent-resource=tap-hooks`, key `device_ips`).

### Known failures

**tap-hooks CrashLoopBackOff: `open /run/tap/trigger/token: permission denied`.** Secret volumes mounted `0440` are owned by root; a non-root pod reads them through `fsGroup`. Any Deployment that mounts a `0440` secret and runs as 65532 needs `fsGroup: 65532`.

**`agent:deploy`: `rule 6: runner.image … is not a curated runner image`.** The runners were re-pinned in `platform.yaml` (usually by a full `images:push`). Pin the old digests back, or bump and re-test every agent; see "Re-pin the runners".

**`agent:deploy`: `error: no objects passed to apply`.** `tapctl render` failed; the real error is the line above it.

**Agent pod crashes after a deploy with an unknown flag.** The rendered manifest needs a newer harness than the one pinned. `task images:push IMAGES=harness`, commit, redeploy.

**`registry:login` fails, or `docker push` gets 401 although `oras login` succeeded.** zot allows anonymous reads, so `oras login` succeeds with any password. `registry:login` now checks the password against `/v2/` first. Regenerate: `rm .tap/registry.pass && task registry:creds --force`, then restart zot.

**`openbao:onboard` says "onboarding failed" after creating the keys.** `bao … -field=token` prints no trailing newline; anything that reads it line by line needs `printf '%s\n'`. Re-running onboard is safe: existing keys are kept.

**`.tap/` is missing.** Nothing in it is irreplaceable. Digests (`.tap/<agent>.digest`) can be recovered from the registry; public keys come from `platform.yaml` (`task signing:pubs`); the registry password is regenerated as above; the operator token is in the Keychain.

**A deploy hangs at admission, or audit reports a timeout.** Kyverno stalls on some uncached registry lookups (phase 7). In audit mode failures are ignored after 5s; retry the deploy.

**A service change or a `platform.yaml` edit isn't live.** Services and their `platform.yaml` reach the cluster only from main: the platform workflow builds the images whose inputs changed, publishes `tap/platform-manifests:main`, and Flux applies it (`task flux:status`). The commit status `tap/deploy` turns green once Flux has applied the commit and every service is ready; the run's `deploy` job says what it was waiting for. A pushed image (`task images:push`) or a local edit doesn't deploy anything by itself.

**Flux is down, or a fix can't wait for CI.** `task platform:apps FORCE=1` renders the apps layer from your checkout at the images' input tags and applies it. The images must already be in zot (`task images:push IMAGES=<name>`). Flux reverts it at its next reconcile, so merge the same change, or `flux suspend kustomization platform` while you work (and `resume` after).
