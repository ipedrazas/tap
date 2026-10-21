# tap

Agentic Platform: agents as signed OCI bundles on one generic harness. An agent is data: a manifest (`agent.yaml`), a system prompt, skills, tool scripts and fixtures. It runs in a gVisor pod next to a runner that executes its declared tools and nothing else.

- Architecture: [docs/architecture.md](docs/architecture.md); operating it: [docs/runbook.md](docs/runbook.md)
- Bundles and signing: [docs/bundles-and-signing.md](docs/bundles-and-signing.md)
- Design: [docs/Agent-as-OCI + Harness Design.md](docs/Agent-as-OCI%20+%20Harness%20Design.md)
- Implementation plan and findings: [docs/implementation-plan.md](docs/implementation-plan.md)
- CLI reference: [docs/cli.md](docs/cli.md) (also `tapctl help <command>`)
- Releases: a `v*` tag publishes `tapctl`, `tap-runner` (linux/amd64, linux/arm64, darwin/arm64), `platform.yaml` and checksums as a GitHub release, which the agents repo pins
- Agents live in [ipedrazas/tap-agents](https://github.com/ipedrazas/tap-agents), with the `new-agent` skill for writing them; it pins a release of this repo

## Components

| Binary | Runs | Job |
| --- | --- | --- |
| `tapctl` | your machine, CI | scaffold, validate, diff permissions, build and push bundles, attest tested bundles, render manifests and admission policies, list running agents |
| `tap-harness` | agent pod | model loop through the AI gateway; never runs tools |
| `tap-runner` | agent pod (sidecar) | runs declared script tools and remote MCP tools, each tool as its own uid |
| `tap-egress-proxy` | `tap-system` | the only way out for tool traffic; per-call, per-tool host allowlists |
| `tap-console` | `tap-system` | view of running agents at https://tap.hiddenfield.dev, plus the factory's form |
| `tap-hooks` | `tap-system` | the only public endpoint: receives hook deliveries (GitHub webhooks, CI), verifies them and starts agent turns |
| `tap-factory` | `tap-factory` | turns a spec into a PR with the `new-agent` skill (pi-durable, TypeScript); no registry, cluster or GitHub credentials |
| `tap-factory-publisher` | `tap-factory` (sidecar) | the only holder of the GitHub token; opens PRs that add one `agents/<name>/` directory |

Every binary reports its build with `--version` (version, git commit, build date, dirty flag).

## Quickstart

```bash
task tools                # oras, crane, cosign
task tapctl               # builds bin/tapctl with version info

bin/tapctl agent ls                       # what's running
bin/tapctl agent get demo-agent
```

Agents are written and checked in [tap-agents](https://github.com/ipedrazas/tap-agents), checked out next to this repo (`AGENTS_DIR`, default `../tap-agents/agents`):

```bash
cd ../tap-agents
task agent:new NAME=demo-agent RUNNER=runner-python DESCRIPTION="..."
task agent:validate AGENT=demo-agent
task agent:test:local AGENT=demo-agent    # fast, host interpreters
task agent:test AGENT=demo-agent          # forwarded here: real runner image under gVisor; signs + attests a passing run
task agent:dev AGENT=demo-agent           # forwarded here: validate, test, deploy
```

Bundles and curated images are signed with the pipeline's two keys, `tap-build` and `tap-tested`. These are Transit keys in OpenBao (`tap-transit/`): they are not exportable, and cosign signs through `hashivault://`. Their public halves are in `platform.yaml` `signing.*`. You authenticate with the operator token that `task openbao:onboard` stores in the macOS Keychain (`deploy/openbao/token.sh`; `TAP_BAO_TOKEN` overrides it). Nothing in `.tap/` is a credential that can't be regenerated. Admission checks every agent pod: curated images and the bundle come from the registry by digest and are signed, and the bundle is signed as tested. See phase 7 in the implementation plan.

To have the factory write an agent, open https://tap.hiddenfield.dev/factory (or run `task factory:submit NAME=<name>-agent SPEC=spec.md`).
1. It drafts a brief first: what people give the agent, each tool and where its inputs come from, hosts, secrets, effects and examples.
2. It asks you about any gaps on the factory page.
3. It builds exactly the brief you approved, tests it, and opens a PR whose description is its report.

The PR goes to tap-agents. Store its declared secrets first (`pbpaste | task secrets:put AGENT=<name> NAME=<NAME>`). Once the PR is merged, tap-agents' publish workflow builds, tests and signs the bundle in CI. It publishes the signed manifests, and Flux applies them. The merge commit gets a `tap/deploy` status once Flux has applied it and the agent is ready. See `docs/gitops-plan.md`.

Agents are served at `https://<name>.a.hiddenfield.dev` behind Dex. Declared secrets come from OpenBao through External Secrets: store each one with `pbpaste | task secrets:put AGENT=<name> NAME=<NAME>` before deploying. `.env.<agent>` (`NAME=value`, git-ignored) is only for local `tapctl mcp call`.

Each agent has a settings page in the console (`https://tap.hiddenfield.dev/agents/<name>`). Members of the agent's owning team (`teams` in `platform.yaml`, keyed by `metadata.owner`; edits reach the console when they merge to main) can:

- set a display title, stored in `tap-system/agent-settings`. If it's empty, the console shows `metadata.title` and then the name.
- set secret values. These go to OpenBao as role `tap-console`, which can write but not read values back. The ExternalSecret is then force-synced.
- edit tool egress and `metadata.title`. This opens a PR on `agent.yaml` with a patch version bump, through the factory's publisher, and takes effect after review and `agent:launch`.

- add hooks, see below.
- see the agent's run history (`/agents/<name>/runs`): every chat and hook run, with each model reply and tool call, read from the session traces in the sessions bucket with a read-only key (`tap-system/sessions-reader`, from `task sessions:setup`). Traces hold prompts and tool output, so only the owning team sees them. Callers are stored as pseudonyms; the console names the ones it can recognise (you, team members, the agent's hooks).

The OpenBao role comes from `task openbao:onboard`.

## Hooks

A hook lets an external system start a run of an agent: a GitHub webhook on pull requests for `gh-pr-agent`, a CI job after a deploy. Owners add hooks on the agent's console page. Each hook has:

- a URL, `https://hooks.hiddenfield.dev/h/<agent>/<hook>` (platform.yaml `hooks.publicURL`);
- its own credential, shown once (and replaceable): a GitHub webhook secret, checked against `X-Hub-Signature-256`, or a bearer token (`taphk_…`), of which only the SHA-256 is stored;
- an event filter (`pull_request`, `pull_request.opened`; matched against `X-GitHub-Event` or `X-Tap-Event` and the payload's `action`);
- a prompt template over the delivery, e.g. `Review {{.payload.pull_request.html_url}}`. `printf`, `define`/`block`/`template` and nested `range` are refused, so a template cannot stall tap-hooks; a field under a missing one fails the delivery (guard it with `{{with}}`).

`tap-hooks` verifies each delivery, ignores `ping`s and filtered events, drops redeliveries (same delivery ID or same body) of a run it has started, limits each hook to 30 deliveries a minute, renders the prompt and calls the agent's trigger port (8081, reachable only from tap-hooks, never routed by the gateway, and requiring the token in `tap-system/hooks-trigger-token`, which is mounted into each harness but never the runner). The harness answers 202 at once and runs the turn in a new session as user `hook:<name>`, at most two at a time per agent; the trace is exported like any other session, with turn trigger `hook`. The agent's `effectsPolicy` applies as usual. Payload text is attacker-controllable, so templates should hand the agent URLs and numbers to look up rather than free text.

Settings live in `tap-system/agent-settings` (`<agent>.hook.<name>`), credentials in Secret `tap-system/agent-hooks`, which the console can patch but not read and tap-hooks can only read.

Exposure: only `tap-hooks` leaves the LAN. The Tailscale Kubernetes operator publishes its Service on the tailnet (annotations in `deploy/platform/hooks.yaml`), and Nginx Proxy Manager on the VPS terminates TLS for `hooks.hiddenfield.dev` and proxies `/h/` to `http://tap-hooks` over the tailnet.

## Task reference

`task --list` shows everything. The main groups:

- `platform:*`: bring up `tap-system` (gateway, certificates, registry, egress proxy, console)
- `images:push`, `images:sign`: build, sign and pin the harness, runner and collector images (CI builds the platform services)
- `secrets:*`, `openbao:onboard`: External Secrets, tap's OpenBao setup, storing agent secrets and checking their isolation
- `signing:key`, `signing:pin`, `admission:*`: the Transit signing keys' public halves, pinning them in `platform.yaml`, Kyverno, and the admission policies (`platform.yaml` `signing.admission` picks audit or enforce)
- `agent:*`: the agent lifecycle (`new`, `validate`, `diff`, `test`, `push`, `deploy`, `dev`, `launch`, `logs`, `destroy`, `mcp-*`)
- `factory:*`: deploy the factory (`secrets`, `github-token`, `deploy`), submit specs and list jobs, and `factory:eval` to rerun the eval specs
- `isolation:test`: deploy `probe-agent` and check that tools can't read secrets or reach undeclared networks
- `docs:cli`: regenerate `docs/cli.md`

CI (`.github/workflows/ci.yml`) runs on every PR: gofmt, `go vet`, `go test` and the factory's typecheck and tests. Agents are validated, diffed and fixture-tested in tap-agents' own CI, with the pinned release. The in-cluster `agent:test`, signing and deploys stay local for now. The keys are in OpenBao, so a self-hosted runner only needs a token with `tap-signer` and/or `tap-tested-signer`.

## Repository layout

```
testdata/agents three agents the Go tests use as fixtures (the real ones are in tap-agents)
cmd/             tapctl, harness, runner, egress-proxy, console, factory-publisher
factory/         the factory service (TypeScript on pi-durable) and its own eval specs (agents' are in tap-agents)
pkg/             spec (schema + rules), bundle, render, attest, runner, harness, egress, mcp, inventory, scaffold, factory (publisher), version
schema/          agent.v1.json, fixture.v1.json
images/          Dockerfiles for the curated images
deploy/platform  tap-system manifests (kustomize): infra/ (task platform:up), apps/ (Flux, from main)
deploy/factory   tap-factory manifests (kustomize): infra/ (task factory:deploy), apps/ (Flux, from main)
deploy/apps.sh   input tags for the apps layer's images, and its render
deploy/kyverno   Kyverno Helm values (admission)
deploy/openbao   OpenBao onboarding, policies and the in-cluster bao runner
deploy/external-secrets  External Secrets Helm values
platform.yaml    registry, domain, model routes, pinned image digests
```
