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
- [ ] Move `model-credentials`, `trigger-token` and `sessions-credentials` to OpenBao under `secret/tap/shared/<name>`. Add a `tap-shared` policy readable by every agent namespace's `tap-secrets` account, alongside the existing per-namespace one. Render one ExternalSecret per secret. Pod mounts already decide which container sees which secret, so this doesn't widen access.
- [ ] `runner-token`: an ESO `Password` generator plus an ExternalSecret with `refreshInterval: 0`, so the value doesn't change on every sync.
- [ ] `egress-key`: generated the same way, then `PushSecret` to `secret/tap/egress-keys/<agent>`. `tap-system/egress-keys` becomes an ExternalSecret that collects every key with `dataFrom.find`. Check that the proxy picks up new keys without a restart.
- [ ] Egress allowlists: `tapctl render` emits `ConfigMap tap-system/egress-policy-<agent>` (label `tap.hiddenfield.dev/egress-policy=<agent>`). The proxy watches ConfigMaps by label instead of reading the single `egress-policy` one. The factory's `tap-factory.json` moves to its own labelled ConfigMap in `deploy/factory`.
- [ ] `agent:secrets` shrinks to waiting for ExternalSecrets to be ready. `agent:deploy` becomes `tapctl render | kubectl apply` plus that wait.
- [ ] Golden render tests cover the new objects; `isolation:test` and `secrets:check` still pass.
- **Done when:** after `kubectl delete ns agent-echo-agent`, `tapctl render … | kubectl apply -f -` alone brings echo-agent back, it can make tool calls, and hooks work.

### Phase 2: tapctl releases
- [ ] A `release` workflow in this repo: on a `v*` tag, build tapctl for linux/amd64 and darwin/arm64 and attach the binaries, their checksums and `platform.yaml` to the GitHub release.
- [ ] `tapctl render`, `validate`, `diff`, `attest` and `admission` already take `--platform`. Check that every command `tap-agents` CI uses runs outside this repo's checkout. `agent:launch` and the `.git`-based diffs assume this repo today.
- **Done when:** `tap-agents` CI downloads `tapctl` and `platform.yaml` for `v0.1.0` and validates an agent.

### Phase 3: Agents move to `tap-agents`
- [ ] Copy `agents/` with history (`git filter-repo --subdirectory-filter agents`, or start fresh and link back to this repo). Add `tap.version`, a Taskfile with the agent tasks and the PR workflow (validate, permission diff, local fixtures), taken from this repo's `agents` CI job. No secrets, no Tailscale, no OIDC on `pull_request`, and never use `pull_request_target`.
- [ ] Branch protection on `main`, plus a CODEOWNERS entry per agent generated from `platform.yaml` `teams` and each agent's `metadata.owner`, so owning teams review their own agents.
- [ ] The factory: `FACTORY_REPO` and the publisher's `--repo` point at `tap-agents`. The factory reads `.claude/skills/new-agent/SKILL.md` and validates against tapctl from its checkout, so either the skill moves to `tap-agents` or the factory clones both repos. Moving the skill is simpler, because it's about writing agents. The fine-grained GitHub token needs access to the new repo.
- [ ] The console: egress-edit PRs go through the publisher, so they follow the repo change. Check `GET /console/agents/{name}` too.
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
  1. `build`: for each agent changed since the last successful run, `tapctl bundle build --push`, then sign with `tap-build`.
  2. `test` (environment `tested`): run the fixtures Job in `tap-ci`, then `tapctl attest`, `cosign attest` (build key) and `cosign sign` (tested key). This is the only job that can sign as tested.
  3. `publish`: for every agent, build the bundle without pushing (the digest is reproducible), require both signatures with `cosign verify`, then `tapctl render` all agents and `flux push artifact` the manifests to `tap/agents-manifests:<sha>`. Tag the artifact `main` and sign it with `tap-build`. The job fails if any agent's digest isn't tested-signed.
- **Done when:** a version bump of echo-agent merged to `main` produces a signed, tested bundle and a signed manifests artifact. Nobody's laptop is involved, and `task agent:verify` passes on the result.

### Phase 5: Flux applies
- [ ] Install Flux from manifests kept in this repo (`deploy/flux`), not `flux bootstrap`, which writes to a repo. Only the source and kustomize controllers are needed, plus notification controller for alerts.
- [ ] Add `OCIRepository agents` for `tap/agents-manifests:main`, with `verify: {provider: cosign, secretRef: tap-build-pub}`. The public key comes from `platform.yaml` `signing.publicKey`, rendered by `tapctl`.
- [ ] Add a `Kustomization agents` with `prune: true`, `wait: true` and a timeout. Take over the existing agents with server-side apply. No pod restarts are expected when the rendered objects are unchanged.
- [ ] Flux's service account needs admission like everyone else. Agent namespaces keep `tap.hiddenfield.dev/admission=verify`, so Kyverno sees Flux's applies.
- [ ] After `publish`, the smoke chat runs in CI: wait for `Kustomization agents` to report the new revision ready, then send one chat per changed agent (same check as `agent:launch`).
- [ ] Drift: a `kubectl edit` to an agent's Deployment is reverted on the next reconcile. A notification-controller alert reports failed reconciles. Where it goes (console banner or a webhook) is an open question below.
- **Done when:** a merged PR goes live with no manual step; `git revert` + merge rolls it back; deleting an agent's directory removes its namespace.

### Phase 6: Later
- [ ] **Enforce admission.** Blocked on the Kyverno stall (phase 7 of the implementation plan). Once Flux is the only way to deploy, enforcing becomes more important: until then, write access to `tap-agents` `main` is effectively deploy access.
- [ ] **Auto-merge PRs that don't widen permissions** (design doc, Lifecycle step 5): green CI plus `tapctl diff` exit 0 means auto-merge. New agents always widen permissions, so they always need review.
- [ ] Check the permission diff and the attestation's agent name at admission (known gaps in phase 7).
- [ ] Move `deploy/platform` and `deploy/factory` to Flux the same way.

## Dev loop

`task agent:dev` pushes a `:dev` bundle and applies it into `agent-<name>`, which Flux would revert. Options:

1. **Preview namespaces** (recommended): `agent:dev` deploys to `agent-<name>-dev`, which Flux doesn't manage. It's the same render with a different namespace and hostname (`<name>-dev.a.hiddenfield.dev`). The `:dev` bundle still needs both signatures to be admitted, so the operator keeps `tap-signer` and runs fixtures locally. That needs a dev-only tested key, or admission allowing build-only bundles in `*-dev` namespaces. To decide.
2. Suspend the agent's Kustomization while developing (`flux suspend`). That would mean one Kustomization per agent, and suspends are easy to forget.

## Open questions

- **egress-key round trip.** Generating in the agent namespace, then `PushSecret` to OpenBao, then collecting in `tap-system` has three moving parts. The alternative is to let the proxy read `egress-key` Secrets from agent namespaces directly. That needs cluster-wide list/watch on Secrets, which is broader than we want. Is the round trip worth it?
- **Shared secrets in agent namespaces.** Copying the model key into every namespace is today's model too. Do we want per-agent model keys from the AI gateway instead, while we're here?
- **Where Flux alerts go:** the console, a GitHub commit status on `tap-agents`, or both.
- **Changed-agent detection** in the `build` job: a git diff since the last successful run, or rebuilding everything every time and relying on the skip-if-signed shortcut. The second is simpler and costs a fixtures Job per agent per merge.
- Does the factory's eval set (which reads reference agents from its checkout) need to change once agents live in their own repo?
