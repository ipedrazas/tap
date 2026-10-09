# tap

Agentic Platform: agents as signed OCI bundles on one generic harness. An agent is data: a manifest (`agent.yaml`), a system prompt, skills, tool scripts and fixtures. It runs in a gVisor pod next to a runner that executes its declared tools and nothing else.

- Design: [docs/Agent-as-OCI + Harness Design.md](docs/Agent-as-OCI%20+%20Harness%20Design.md)
- Implementation plan and findings: [docs/implementation-plan.md](docs/implementation-plan.md)
- CLI reference: [docs/cli.md](docs/cli.md) (also `tapctl help <command>`)
- Writing agents: [.claude/skills/new-agent/SKILL.md](.claude/skills/new-agent/SKILL.md)

## Components

| Binary | Runs | Job |
| --- | --- | --- |
| `tapctl` | your machine, CI | scaffold, validate, diff permissions, build and push bundles, attest tested bundles, render manifests and admission policies, list running agents |
| `tap-harness` | agent pod | model loop through the AI gateway; never runs tools |
| `tap-runner` | agent pod (sidecar) | runs declared script tools and remote MCP tools, each tool as its own uid |
| `tap-egress-proxy` | `tap-system` | the only way out for tool traffic; per-call, per-tool host allowlists |
| `tap-console` | `tap-system` | view of running agents at https://tap.hiddenfield.dev, plus the factory's form |
| `tap-factory` | `tap-factory` | turns a spec into a PR with the `new-agent` skill (pi-durable, TypeScript); no registry, cluster or GitHub credentials |
| `tap-factory-publisher` | `tap-factory` (sidecar) | the only holder of the GitHub token; opens PRs that add one `agents/<name>/` directory |

Every binary reports its build with `--version` (version, git commit, build date, dirty flag).

## Quickstart

```bash
task tools                # oras, crane, cosign
task tapctl               # builds bin/tapctl with version info

task agent:new NAME=demo-agent RUNNER=runner-python DESCRIPTION="..."
task agent:validate AGENT=demo-agent
task agent:test:local AGENT=demo-agent    # fast, host interpreters
task agent:test AGENT=demo-agent          # real runner image under gVisor; signs + attests a passing run
task agent:verify AGENT=demo-agent        # what admission will check
task agent:dev AGENT=demo-agent           # validate, test, deploy

bin/tapctl agent ls                       # what's running
bin/tapctl agent get demo-agent
```

Bundles and curated images are signed with the pipeline's cosign keys (in `.tap/`, git-ignored). Admission checks every agent pod: curated images and the bundle come from the registry by digest and are signed, and the bundle is signed as tested. See phase 7 in the implementation plan.

To have the factory write an agent, open https://tap.hiddenfield.dev/factory (or run `task factory:submit NAME=<name>-agent SPEC=spec.md`).
1. It drafts a brief first: what people give the agent, each tool and where its inputs come from, hosts, secrets, effects and examples.
2. It asks you about any gaps on the factory page.
3. It builds exactly the brief you approved, tests it, and opens a PR whose description is its report.

Once the PR is merged, `task agent:launch AGENT=<name>` takes it live. It checks that main is current and the secrets are in OpenBao, runs `agent:test`, deploys, and runs a smoke chat. See phase 9 in the implementation plan.

Agents are served at `https://<name>.a.hiddenfield.dev` behind Dex. Declared secrets come from OpenBao through External Secrets: store each one with `pbpaste | task secrets:put AGENT=<name> NAME=<NAME>` before deploying. `.env.<agent>` (`NAME=value`, git-ignored) is only for local `tapctl mcp call`.

## Task reference

`task --list` shows everything. The main groups:

- `platform:*`: bring up `tap-system` (gateway, certificates, registry, egress proxy, console)
- `images:push`, `images:sign`: build, sign and pin the harness, runner, proxy and console images
- `secrets:*`, `openbao:onboard`: External Secrets, tap's OpenBao setup, storing agent secrets and checking their isolation
- `signing:key`, `admission:*`: the pipeline's cosign keys, Kyverno, and the admission policies (`platform.yaml` `signing.admission` picks audit or enforce)
- `agent:*`: the agent lifecycle (`new`, `validate`, `diff`, `test`, `push`, `deploy`, `dev`, `launch`, `logs`, `destroy`, `mcp-*`)
- `factory:*`: deploy the factory (`secrets`, `github-token`, `deploy`), submit specs and list jobs, and `factory:eval` to rerun the eval specs
- `isolation:test`: deploy `probe-agent` and check that tools can't read secrets or reach undeclared networks
- `docs:cli`: regenerate `docs/cli.md`

CI (`.github/workflows/ci.yml`) runs on every PR: gofmt, `go vet`, `go test`, the factory's typecheck and tests, and for each agent `validate`, the permission diff against the base branch (widening is a warning plus a summary entry for review) and `agent:test:local` with the runner images' Node and Python versions. The in-cluster `agent:test`, signing and deploys stay local until the keys move to OpenBao.

## Repository layout

```
agents/          one directory per agent bundle
cmd/             tapctl, harness, runner, egress-proxy, console, factory-publisher
factory/         the factory service (TypeScript on pi-durable) and its eval specs
pkg/             spec (schema + rules), bundle, render, attest, runner, harness, egress, mcp, inventory, scaffold, factory (publisher), version
schema/          agent.v1.json, fixture.v1.json
images/          Dockerfiles for the curated images
deploy/platform  tap-system manifests (kustomize)
deploy/factory   tap-factory manifests (kustomize)
deploy/kyverno   Kyverno Helm values (admission)
deploy/openbao   OpenBao onboarding, policies and the in-cluster bao runner
deploy/external-secrets  External Secrets Helm values
platform.yaml    registry, domain, model routes, pinned image digests
```
