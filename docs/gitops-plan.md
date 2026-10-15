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
- [ ] Copy `agents/` with history (`git filter-repo --subdirectory-filter agents`, or start fresh and link back to this repo). Add `tap.version`, a Taskfile with the agent tasks and the PR workflow (validate, permission diff, local fixtures), taken from this repo's `agents` CI job. No secrets, no Tailscale, no OIDC on `pull_request`, and never use `pull_request_target`.
- [ ] Branch protection on `main`, plus a CODEOWNERS entry per agent generated from `platform.yaml` `teams` and each agent's `metadata.owner`, so owning teams review their own agents.
- [ ] The factory: `FACTORY_REPO` and the publisher's `--repo` point at `tap-agents`. The factory reads `.claude/skills/new-agent/SKILL.md` and validates against tapctl from its checkout, so either the skill moves to `tap-agents` or the factory clones both repos. Moving the skill is simpler, because it's about writing agents. The fine-grained GitHub token needs access to the new repo.
- [ ] The console: egress-edit PRs go through the publisher, so they follow the repo change. Check `GET /console/agents/{name}` too.
- [ ] **Evals move in with their agents** (decided Oct 15). Each eval spec with a reference agent becomes `agents/<name>/eval.md`, and its `reference:` is implied by the directory. Specs without a reference agent (`pr-review.md`, and `pr-review-prompt.md`, which is bad on purpose) test the factory rather than an agent, so they stay in `factory/evals`. `factory:eval` reads `agents/*/eval.md` from a `tap-agents` checkout at `main` as well as its local specs. `run.ts` stays here because it imports the factory's `permissions.ts`.
  - The answer key stays hidden. The factory clones `FACTORY_REPO` and deletes the hidden agent's directory before the model starts, which now takes the eval spec with it. Nothing else changes once `FACTORY_REPO` points at `tap-agents`.
  - Bundles are unaffected. `tapctl bundle build` copies only what `agent.yaml` declares, so `eval.md` stays out of the bundle and its digest.
  - Later: the factory could write the approved brief as `eval.md` in every PR it opens, so each agent it creates adds a case to the eval set.
- [ ] Remove `agents/` and the `agents` CI job from this repo, and update README, `architecture.md` and `runbook.md`.
- **Done when:** the factory opens a PR on `tap-agents`, its CI passes, and `task agent:launch` from a `tap-agents` checkout still deploys it. The manual path still works at this stage.

### Phase 4: CI builds, tests and signs
- [ ] **Tailscale:** an OAuth client limited to `tag:tap-ci`. In the tailnet ACLs, `tag:tap-ci` can reach only zot `:443`, OpenBao `:8443` and the Kubernetes API. Expose whichever of these aren't already on the tailnet (subnet route or the Tailscale operator, which is already installed in `tailscale`).
- [ ] **Kubernetes:** a `tap-ci-runner` service account that can create and read Jobs and Pods (including logs) in `tap-ci` and nothing else. CI gets a short-lived token for it. Long-lived kubeconfigs are out.
- [ ] **zot:** a `tap-ci` user that can push only to `agents/*` and `tap/agents-manifests`.
- [ ] **OpenBao:** `auth/jwt-github` trusting `token.actions.githubusercontent.com`, with two roles:
  - `tap-ci-build`: policy `tap-signer`, bound to `repository=ipedrazas/tap-agents`, `ref=refs/heads/main`;
  - `tap-ci-tested`: policy `tap-tested-signer`, bound to the same plus `environment=tested`.
  - Take `tap-tested-signer` off the operator token. Keep `tap-signer` on it for curated images, which are still signed from the laptop.
- [ ] **Workflow on push to `main`** (one concurrency group, so runs never overlap):
  1. `plan`: for every agent, `tapctl bundle build` without `--push` prints the digest the sources build to. Any digest without a valid tested signature (`cosign verify --key <tested pub>`) needs work, and goes to the next two jobs as a matrix. If there are none, skip to `publish`: a merge that only touched docs or a harness bump still re-renders.
  2. `build`: for each of those agents, `tapctl bundle build --push`, then sign with `tap-build`.
  3. `test` (environment `tested`): run the fixtures Job in `tap-ci`, then `tapctl attest`, `cosign attest` (build key) and `cosign sign` (tested key). This is the only job that can sign as tested.
  4. `publish`: require both signatures on every agent's digest, then `tapctl render` all agents and `flux push artifact` the manifests to `tap/agents-manifests:<sha>`. Tag the artifact `main` and sign it with `tap-build`. The job fails if any agent's digest isn't tested-signed.
- **Done when:** a version bump of echo-agent merged to `main` produces a signed, tested bundle and a signed manifests artifact. Nobody's laptop is involved, and `task agent:verify` passes on the result.

### Phase 5: Flux applies
- [ ] Install Flux from manifests kept in this repo (`deploy/flux`), not `flux bootstrap`, which writes to a repo. Only the source and kustomize controllers are needed, plus notification controller for alerts.
- [ ] Add `OCIRepository agents` for `tap/agents-manifests:main`, with `verify: {provider: cosign, secretRef: tap-build-pub}`. The public key comes from `platform.yaml` `signing.publicKey`, rendered by `tapctl`.
- [ ] Add a `Kustomization agents` with `prune: true`, `wait: true` and a timeout. Take over the existing agents with server-side apply. No pod restarts are expected when the rendered objects are unchanged.
- [ ] Flux's service account needs admission like everyone else. Agent namespaces keep `tap.hiddenfield.dev/admission=verify`, so Kyverno sees Flux's applies.
- [ ] After `publish`, the smoke chat runs in CI: wait for `Kustomization agents` to report the new revision ready, then send one chat per changed agent (same check as `agent:launch`).
- [ ] **Commit status:** the same job posts `tap/deploy` on the merged commit in `tap-agents`: pending at `publish`, then success when the smoke chats pass, or failure with the agent and reason. It uses the workflow's own `GITHUB_TOKEN` (`permissions: statuses: write`), so no extra token is needed. Flux's GitHub provider could post statuses too, but our source is an OCI artifact rather than a git commit, and it would need its own token.
- [ ] **Alerts:** a notification-controller `Alert` on failed reconciles of `Kustomization agents`, sent to a `generic-hmac` provider on a new console endpoint, which shows a banner on the agent's page. This covers failures outside a deploy, such as drift that can't be reverted or an ExternalSecret that stopped syncing.
- [ ] Drift: a `kubectl edit` to an agent's Deployment is reverted on the next reconcile.
- **Done when:** a merged PR goes live with no manual step; `git revert` + merge rolls it back; deleting an agent's directory removes its namespace.

### Phase 6: Later
- [ ] **Enforce admission.** Blocked on the Kyverno stall (phase 7 of the implementation plan). Once Flux is the only way to deploy, enforcing becomes more important: until then, write access to `tap-agents` `main` is effectively deploy access.
- [ ] **Auto-merge PRs that don't widen permissions** (design doc, Lifecycle step 5): green CI plus `tapctl diff` exit 0 means auto-merge. New agents always widen permissions, so they always need review.
- [ ] Check the permission diff and the attestation's agent name at admission (known gaps in phase 7).
- [ ] Move `deploy/platform` and `deploy/factory` to Flux the same way.

## Dev loop

Once Flux owns `agent-<name>`, `task agent:dev` can't deploy there: Flux would revert it, and the tested signature only comes from CI after merge. But any change, whether a new agent from the factory or a prompt edit by a person, should run in the cluster before its PR is merged.

Decided (Oct 15): **previews run in `agent-<name>-dev`, and admission accepts build-only bundles there.**

- [ ] `tapctl render --preview` sets the namespace `agent-<name>-dev`, the hostname `<name>-dev.a.hiddenfield.dev` and the label `tap.hiddenfield.dev/preview=true`. Flux never manages these namespaces.
- [ ] Admission: the Kyverno policy requires only the build signature in namespaces labelled `preview=true`, as it already does for `tap-ci`. The native registry and digest policies apply unchanged. The bundle still has to come from our registry, be signed by our build key and run our curated images.
- [ ] `task agent:preview AGENT=<name>` runs from a checkout of the PR branch: validate, push `:dev`, sign with `tap-build`, fixtures in `tap-ci`, deploy to the preview namespace and a smoke chat. `task agent:preview:rm` deletes it. Previews are manual for now. Automating them from PR CI would hand the build key and the cluster to code on a PR branch.
- [ ] Secrets: the OpenBao policy is templated on the service account's namespace, so a preview reads `secret/tap/agent-<name>-dev/*`, never the real agent's values. `agent:preview` checks they're present, like `agent:launch` does, and `secrets:put` takes `PREVIEW=true`. That's the right default for unreviewed code: use test credentials where they exist.
- [ ] Clean-up: previews older than seven days are listed by `tapctl agent ls`. Delete them by hand, or later with a CronJob.

What this allows, to be added to [bundles-and-signing.md](bundles-and-signing.md) when it ships: a bundle that is built but not tested can run, with the agent's real secrets and egress, in a `*-dev` namespace. Creating such a namespace still needs cluster admin, so only the operator can start a preview.

## Parked

- **Per-agent model keys** from the AI gateway, instead of copying the shared key into every namespace. If we do it, the key becomes one of the agent's secrets, settable from the console's settings page like the others (parked Oct 15).
