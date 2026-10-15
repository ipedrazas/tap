# tap: GitOps plan

Merging an agent's PR should be enough to deploy it. Today, someone has to run `task agent:launch` from the operator's machine after the merge. This plan moves agents to their own repo (`ipedrazas/tap-agents`), builds and signs bundles in GitHub Actions and lets Flux apply the result. For how bundles are built, tested and signed today, see [bundles-and-signing.md](bundles-and-signing.md).

## Goals and non-goals

- **Merge = deploy.** A merged change to `agents/<name>/` reaches the cluster without anyone running a task.
- **The PR stays the gate.** It's the only place where a person approves an agent's hosts, secrets and effects. "Deploy without a PR" isn't a goal. Auto-merging PRs that don't widen permissions is a later step (phase 6).
- **Separate the signers.** Today one operator token can sign with both keys. After this plan, only the CI job that ran the fixtures can use `tap-tested`, and no long-lived signing credential exists anywhere.
- **Revert = rollback.** Reverting a commit in `tap-agents` puts the previous bundle back. Bundles are reproducible, so this gives the same digest, and its signatures already exist.
- Not in scope: platform services (`deploy/platform`, `deploy/factory`) stay in this repo and keep their manual `task platform:up`. They can move to Flux later in the same way.

## Decisions (proposed)

| Topic | Proposal | Why |
| --- | --- | --- |
| Agent sources | `ipedrazas/tap-agents`, one `agents/<name>/` per agent, as today | Agents change more often than the platform. The factory, console and CI get write access to agents only. |
| Link to the platform | `tap-agents` pins a tap release (`tap.version`): tapctl and `platform.yaml` come from that tag | Rule 6 ties agents to the runner digests in `platform.yaml`. Bumping the pin is one PR that retests every agent, as a runner change should. |
| GitOps agent | Flux | `OCIRepository` verifies a cosign signature before anything is applied. It's a few small controllers with no UI to run; the console already covers that. Argo CD can't verify OCI sources and brings a UI and RBAC model we don't need. |
| What Flux reads | Rendered manifests for all agents, pushed by CI as a signed OCI artifact (`registry.hiddenfield.dev/tap/agents-manifests`) | CI never commits to git, so there are no bot commits and no CI loops. Flux applies only what the build key signed. The source of truth is still the git history of `tap-agents`. |
| CI | GitHub-hosted runners joined to the tailnet with `tailscale/github-action` (ephemeral node, `tag:tap-ci`) | CI needs zot, OpenBao Transit and the Kubernetes API (the fixtures run in `tap-ci`). Both repos are public, so we don't want a self-hosted runner. |
| CI credentials | OpenBao JWT auth on GitHub's OIDC tokens; no OpenBao token stored in GitHub | Roles bind to repo, branch and environment, so only `main` can sign, and only the job that tested can use `tap-tested`. |
| Per-agent secrets and shared objects | All rendered by `tapctl render`, with nothing created imperatively (phase 1) | Flux can only apply what's in the artifact. |
| `egress-key` | Generated in the agent namespace, pushed to OpenBao, collected into `tap-system` from there (decided Oct 15) | Three moving parts, but the alternative is a proxy with cluster-wide list/watch on Secrets. |
| What CI rebuilds | Every agent's bundle is built locally on each merge; only digests without a tested signature are pushed and tested (decided Oct 15) | Bundles are reproducible, so the digest is the change detection: no git diffs, and a `tap.version` bump that changes an agent's runner is caught automatically. Local builds take seconds; fixtures Jobs only run for new content. |
| Pre-merge previews | `agent-<name>-dev` namespaces, outside Flux; admission accepts build-only bundles there (decided Oct 15, see Dev loop) | PRs, whether from the factory or a person, need a place to run before merge, and the tested key must stay post-merge only. |
| Deploy feedback | A GitHub commit status on `tap-agents` per deploy, posted by CI, and Flux alerts for failed reconciles in the console (decided Oct 15) | Visible from the PR or the phone, without a terminal. |

## What blocks Flux today

`task agent:deploy` does more than `kubectl apply` of `tapctl render`'s output:

| Step | Today | Must become |
| --- | --- | --- |
| `model-credentials` | copied from `tap-system` by `agent:secrets` | an ExternalSecret from OpenBao (shared path) |
| `trigger-token` | copied from `tap-system` | an ExternalSecret from OpenBao (shared path) |
| `sessions-credentials` | copied from `tap-system` | an ExternalSecret from OpenBao (shared path) |
| `runner-token` | `openssl rand` on first deploy | an ExternalSecret from an ESO `Password` generator |
| `egress-key` | `openssl rand`, then patched into `tap-system/egress-keys` | generated as above, pushed to OpenBao with a `PushSecret`, and read by the proxy from OpenBao through its own ExternalSecret |
| egress allowlist | patched into the shared `tap-system/egress-policy` ConfigMap | one rendered ConfigMap per agent (`egress-policy-<agent>` in `tap-system`, labelled); the proxy watches by label |
| tool secrets | ExternalSecret (already rendered) | unchanged |
| smoke chat | `agent:launch`, from the laptop | a CI step after Flux reports the agent ready (phase 5) |

## Phases

Like the implementation plan, each phase ends with something running on the cluster.

### Phase 1: Declarative agents (in this repo)
Done Oct 15 (#41). `tapctl render | kubectl apply` is the whole deploy.
- [x] `model-credentials`, `trigger-token` and `sessions-credentials` are pushed from `tap-system` to OpenBao `secret/tap-platform/<name>` by PushSecrets (`deploy/platform/openbao-platform.yaml`, role `tap-platform`). Each agent reads them with ExternalSecrets; `tap-agent` may read exactly those three. `tap-platform/` is outside `tap/`, so the console and seeder can't write it.
- [x] `runner-token` and `egress-key`: ESO `Password` generators with `refreshPolicy: CreatedOnce`.
- [x] The egress key is pushed to `secret/tap-egress/agent-<name>` (`deletionPolicy: None`, refreshed every minute). **Changed from the plan:** the proxy reads keys from OpenBao itself (role `tap-egress-proxy`), with no `tap-system/egress-keys` ExternalSecret. ESO's `find` against Vault doesn't return one plain key per agent, and this leaves no shared Secret.
- [x] Egress allowlists are rendered as `tap-system/egress-policy-<agent>`, labelled; the proxy lists them by label. The factory's policy is `egress-policy-tap-factory` (created by `factory:secrets`). Its key stays in the legacy `egress-keys` Secret, which the proxy still reads as a fallback, because keys are looked up by `agent-<name>`.
- [x] `agent:secrets` waits for the ExternalSecrets and PushSecrets and removes legacy entries; `agent:deploy` is render, apply, wait.
- [x] Golden render tests cover the new objects; `isolation:test` 17/17 and `secrets:check` 11/11 (new: shared-secret and egress-key capabilities).
- [x] **Done:** echo-agent's namespace deleted and brought back from render output alone: chat, tool calls, trace uploads through the proxy, and the hook trigger port all work. All eight agents are migrated.

Findings:
- **`CreatedOnce` doesn't keep an existing Secret.** ESO adopts it and writes a newly generated value. Harness and runner read the token at startup, so each agent needed a restart after its first phase 1 deploy.
- **The first push can race the generator.** The PushSecret sent the old key a second before ESO replaced it. Fixed with a one-minute push interval and a forced push in `agent:secrets`.
- **mise shims don't work as fixture interpreters**: the runner gives tools an empty environment. Pass real paths (`/opt/homebrew/bin/…`).

### Phase 2: tapctl releases
- [x] `.github/workflows/release.yml`: on a `v*` tag, run the tests, then build `tapctl` and `tap-runner` (for `agent:test:local`) for linux/amd64, linux/arm64 and darwin/arm64, and attach them with `platform.yaml` and `checksums.txt` to the GitHub release.
- [x] Checked outside this repo (a fresh git repo with two agents and `platform.yaml`): `validate`, `diff --base git:HEAD`, `bundle build`, `render`, `egress policy`, `secrets --paths` and `tap-runner test` all work. Templates and schemas are embedded, `git:` bases resolve relative to whatever repo the agent is in, and the only file needed is `platform.yaml` (`--platform`, default `./platform.yaml`).
- [ ] Not signed yet: binaries are verified by checksum only. Sign `checksums.txt` with `tap-build` once CI has OpenBao (phase 4).
- **Done when:** `tap-agents` CI downloads `tapctl` and `platform.yaml` for `v0.1.0` and validates an agent.

### Phase 3: Agents move to `tap-agents`
- [x] All eight agents copied without history (it stays in this repo's `git log`), plus `tap.version`, `fetch-tap.sh` (pinned release into `.tap/`, checked against the release's checksums) and the PR workflow (validate, permission diff, fixtures with the runner images' Node and Python). No secrets, Tailscale or OIDC; no `pull_request_target`. Five agents rebuild to the digests deployed today.
- [x] **Taskfile split.** `tap-agents` has the authoring tasks (`agent:new`, `validate`, `diff`, `test:local`, `mcp-*`) on `.tap/`. It forwards the cluster tasks (`agent:test`, `verify`, `deploy`, `dev`, `launch`, `secrets:put`) to a tap checkout with `AGENTS_DIR`. Here, those tasks take `AGENTS_DIR` (default `../tap-agents/agents`), and the authoring tasks are gone. **Changed from the plan:** the cluster tasks stay in this repo instead of being copied, because they need its scripts (`deploy/openbao`, signing) and phases 4 and 5 replace them anyway.
- [ ] **CODEOWNERS and branch protection.** `platform.yaml` `teams` lists Dex emails, not GitHub handles, so there's nothing to generate CODEOWNERS from yet; teams need a GitHub field first. Branch protection is not set.
- [x] **The factory.** `FACTORY_REPO` and the publisher's `--repo` point at `tap-agents`. The `new-agent` skill moved there. After the checkout, the factory fetches `platform.yaml` from the tap tag in `tap.version`, through the `codeload.github.com` path its egress policy already allows. The addendum describes the new checkout.
- [x] **The console.** Egress-edit PRs and `GET /console/agents/{name}` go through the publisher, so they follow `--repo`.
- [x] **Evals moved in with their agents** (decided Oct 15). `agents/<name>/eval.md` in tap-agents, with the reference implied by the directory. `pr-review*.md` stay in `factory/evals`. `factory:eval` reads both (`--agents-dir`).
  - The answer key stays hidden. The factory clones `FACTORY_REPO` and deletes the hidden agent's directory before the model starts, which now takes the eval spec with it.
  - Bundles are unaffected. `tapctl bundle build` copies only what `agent.yaml` declares, so `eval.md` stays out of the bundle and its digest; the digests above confirm it.
  - Later: the factory could write the approved brief as `eval.md` in every PR it opens, so each agent it creates adds a case to the eval set.
- [x] Removed `agents/`, the skill and the `agents` CI job from this repo. The Go tests use copies of echo-, weather- and countries-agent in `testdata/agents/`. README, `architecture.md` and `runbook.md` are updated.
- [x] **Done (Oct 15):** the factory opened tap-agents#3 (dice-agent) and its CI passed. `task agent:launch` from tap-agents tested, signed and deployed it, and a chat rolled dice.

### Phase 4: CI builds, tests and signs
- [x] **Tailscale:** workload identity federation, not an OAuth secret. The workflow joins the tailnet as `tag:tap-ci` with a federated client ID and audience (repository variables), so GitHub stores no secret at all. zot (`192.168.2.225`), OpenBao (`192.168.2.131`) and the API server (`192.168.2.221`) are already reachable through the subnet router, and public DNS returns those LAN addresses. Grants allow `tag:tap-ci` only `:443`, `:8443` and `:6443` on them.
- [x] **Kubernetes:** `deploy/ci/rbac.yaml`. `tap-ci-runner` may manage Jobs and read Pods and logs in `tap-ci`. CI gets a 30-minute token from OpenBao's Kubernetes secrets engine (`tap-kubernetes/`), which mints it as `openbao-ci-issuer`; that account may only create tokens for `tap-ci-runner`. No kubeconfig is stored anywhere.
- [x] **zot:** user `tap-ci` may read, create and update `agents/**` and `tap/agents-manifests`, with no deletes. Its password is in OpenBao (`secret/tap-ci/zot`).
- [x] **OpenBao:** `auth/jwt-github` (`deploy/openbao/onboard-ci.sh`, `task ci:onboard`), audience `openbao`. Roles are bound to `repository`, `ref=refs/heads/main` and `job_workflow_ref` (the publish workflow on main):
  - `tap-ci-build`: `tap-signer`, `tap-ci`;
  - `tap-ci-tested`: also `tap-tested-signer`, only with `environment=tested`. It needs `tap-signer` too, because the attestation is signed with the build key.
  - [ ] Take `tap-tested-signer` off the operator token once CI has produced a tested bundle. Until then, `agent:launch` from a laptop still needs it.
- [x] **Workflow** (`tap-agents/.github/workflows/publish.yml`, scripts in `ci/`), on push to `main`, one run at a time:
  1. `plan`: build every agent locally, and list those whose digest has no tested signature. Checked against the registry: all nine current agents come out "tested already".
  2. `build` (per agent): push under `metadata.version` and sign with `tap-build`.
  3. `test` (per agent, environment `tested`): fixtures Job in `tap-ci` (the Job only: the namespace isn't CI's to change), attestation with the permission diff against the previous `main`, tested signature.
  4. `publish`: requires both signatures on every agent's digest, renders all agents, then `flux push artifact` to `tap/agents-manifests:<sha>`, signs it with `tap-build` and tags it `main`.
- [x] **Done (Oct 15):** echo-agent 0.1.3, merged to main, came out of CI signed as built and tested (`8b81cf9c…`). Its attestation is by `github-actions@ipedrazas/tap-agents`, with fixtures 3/3 in `tap-ci`. The manifests artifact `tap/agents-manifests:main` is signed.

Findings:
- **GitHub's immutable OIDC subjects.** tap-agents' `sub` claim is `repo:ipedrazas@32796/tap-agents@1420334254:…`, so a Tailscale credential written for `repo:ipedrazas/tap-agents:*` never matches (403). OpenBao binds `repository`, `ref` and `job_workflow_ref`, which keep the plain names.
- **tailscale/github-action always passes `--accept-routes`**; passing it again in `args` makes `tailscale up` fail.

### Phase 5: Flux applies
- [x] **Flux v2.9.6 from `deploy/flux`** (`task flux:install`), not `flux bootstrap`. Only source-controller, kustomize-controller and notification-controller are included (`trim.yq` drops the rest and their CRDs), with images pinned by digest.
- [x] **`OCIRepository agents`** (`tap/agents-manifests:main`) verifies the cosign signature against `platform.yaml`'s build key (Secret `tap-build-pub`). Key-based verification works offline (`SourceVerified=True`). An unsigned artifact pushed to a test tag was refused (`no matching signatures were found`) and nothing was stored.
- [x] **`Kustomization agents`**: `prune`, `wait`, 5-minute timeout. It applies as `tap-agents-applier`, which can manage only the kinds `tapctl render` emits (no Secrets, RBAC, Pods or exec), instead of kustomize-controller's own cluster-admin account. It took over all nine agents on its first apply and rolled echo-agent to the CI-built 0.1.3.
- [x] Admission: agent namespaces keep their label, so Kyverno sees Flux's applies like any others. `flux-system` isn't labelled, so the native registry policies don't apply to Flux's own ghcr.io images.
- [x] **Commit status:** a `deploy` job after `publish` posts `tap/deploy` (pending, then success or failure) with the workflow's `GITHUB_TOKEN`. It asks Flux to fetch now and waits until the Kustomization's `lastAppliedOriginRevision` is this commit and it's Ready. `tap-ci-runner` can read the Kustomization and annotate the source in `flux-system`, nothing more.
- [ ] **Smoke chat from CI.** Not done: reaching an agent's chat needs `pods/exec` in agent namespaces, or an identity at the Dex-protected gateway, and neither should belong to CI. The readiness probes and `wait` cover "running"; a chat check needs its own identity (later).
- [ ] **Alerts:** a notification-controller `Alert` on failed reconciles, to a console endpoint that shows a banner. Not built yet; `tap/deploy` covers deploys.
- [x] `agent:deploy` / `agent:launch` / `agent:dev` refuse Flux-managed agents (`FORCE=1` overrides until the next reconcile).
- [x] Drift: echo-agent scaled to 2 by hand was back to 1 after the next reconcile.
- [x] **Done (Oct 15):**
  - **Merge deploys:** echo-agent 0.1.4 (tap-agents#10) went live with no manual step.
  - **Revert rolls back:** reverting it (#11) needed no rebuild, because the 0.1.3 sources rebuilt to the already-tested `8b81cf9c…`. Build and test were skipped, and Flux rolled it back.
  - **Removal prunes:** removing dice-agent (#9) pruned its namespace (gone in about 15s, no stuck finalizers) and its egress policy.
  - `tap/deploy` was green on each.

Findings:
- **A job downstream of skipped jobs is skipped too.** `deploy` needs the same `always() && needs.publish.result == 'success'` as `publish`.

### Phase 6: Later
- [ ] **Enforce admission.** Blocked on the Kyverno stall (phase 7 of the implementation plan). Once Flux is the only way to deploy, enforcing becomes more important: until then, write access to `tap-agents` `main` is effectively deploy access.
- [ ] **Auto-merge PRs that don't widen permissions** (design doc, Lifecycle step 5): green CI plus `tapctl diff` exit 0 means auto-merge. New agents always widen permissions, so they always need review.
- [ ] Check the permission diff and the attestation's agent name at admission (known gaps in phase 7).
- [ ] Move `deploy/platform` and `deploy/factory` to Flux the same way.

## Dev loop

Once Flux owns `agent-<name>`, `task agent:dev` can't deploy there: Flux would revert it, and the tested signature only comes from CI after merge. But any change, whether a new agent from the factory or a prompt edit by a person, should run in the cluster before its PR is merged.

Decided (Oct 15): **previews run in `agent-<name>-dev`, and admission accepts build-only bundles there.**

- [x] `tapctl render --preview` deploys the agent as `<name>-dev`:
  - namespace `agent-<name>-dev`, host `<name>-dev.a.hiddenfield.dev`, label `tap.hiddenfield.dev/preview=true`;
  - its own egress policy ConfigMap, egress key path, sessions prefix and OpenBao secret paths.

  `TAP_AGENT` stays `<name>`, because the runner refuses to start when it differs from `agent.yaml`. Flux never manages these namespaces.
- [x] **Egress identity** (found while building this). The runner signs proxy credentials as `<name>`, so a preview's credentials name the real agent. The proxy now tries `<name>-dev`'s key when `<name>`'s doesn't verify, and uses the preview's own policy when it does. The keys are independent, so neither side can use the other's allowlist (`TestProxyPreview`). The alternative, a runner flag for the identity, meant rebuilding the runners and re-pinning every agent.
- [x] **Admission:** `tap-agents` skips namespaces with the preview label; the new `tap-previews` requires the build signature only. The native registry and digest policies apply unchanged.
- [x] `task agent:preview AGENT=<name>` (forwarded from tap-agents) runs from a checkout of the PR branch:
  1. validate, and check the preview's secrets;
  2. push `:dev` and sign it with `tap-build`;
  3. fixtures in `tap-ci` (`agent:fixtures`, shared with `agent:test`);
  4. deploy the preview, wait for ESO, smoke chat (`agent:smoke`, shared with `agent:launch`).

  There is no attestation or tested signature. `task agent:preview:rm` deletes the preview. Previews stay manual: running them from PR CI would hand the build key and the cluster to code on a PR branch.
- [x] Secrets: a preview reads `secret/tap/agent-<name>-dev/*`, never the real agent's values. `agent:preview` checks they're present; `secrets:put ... PREVIEW=1` writes them.
- [ ] Clean-up: list previews older than seven days in `tapctl agent ls`. They show up there today as `<name>-dev`, without an age.

## Parked

- **Per-agent model keys** from the AI gateway, instead of copying the shared key into every namespace. If we do it, the key becomes one of the agent's secrets, settable from the console's settings page like the others (parked Oct 15).
