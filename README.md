# tap

Agentic Platform: agents as signed OCI bundles on one generic harness. An agent is data: a manifest (`agent.yaml`), a system prompt, skills, tool scripts and fixtures. It runs in a gVisor pod next to a runner that executes its declared tools and nothing else.

- Design: [docs/Agent-as-OCI + Harness Design.md](docs/Agent-as-OCI%20+%20Harness%20Design.md)
- Implementation plan and findings: [docs/implementation-plan.md](docs/implementation-plan.md)
- CLI reference: [docs/cli.md](docs/cli.md) (also `tapctl help <command>`)
- Writing agents: [.claude/skills/new-agent/SKILL.md](.claude/skills/new-agent/SKILL.md)

## Components

| Binary | Runs | Job |
| --- | --- | --- |
| `tapctl` | your machine, CI | scaffold, validate, diff permissions, build and push bundles, render manifests, list running agents |
| `tap-harness` | agent pod | model loop through the AI gateway; never runs tools |
| `tap-runner` | agent pod (sidecar) | runs declared script tools and remote MCP tools, each tool as its own uid |
| `tap-egress-proxy` | `tap-system` | the only way out for tool traffic; per-call, per-tool host allowlists |
| `tap-console` | `tap-system` | read-only view of running agents at https://tap.hiddenfield.dev |

Every binary reports its build with `--version` (version, git commit, build date, dirty flag).

## Quickstart

```bash
task tools                # oras, crane, cosign
task tapctl               # builds bin/tapctl with version info

task agent:new NAME=demo-agent RUNNER=runner-python DESCRIPTION="..."
task agent:validate AGENT=demo-agent
task agent:test:local AGENT=demo-agent    # fast, host interpreters
task agent:test AGENT=demo-agent          # real runner image under gVisor
task agent:dev AGENT=demo-agent           # push :dev bundle and deploy

bin/tapctl agent ls                       # what's running
bin/tapctl agent get demo-agent
```

Agents are served at `https://<name>.a.hiddenfield.dev` behind Dex. Secrets for local deploys go in `.env.<agent>` (`NAME=value`, git-ignored).

## Task reference

`task --list` shows everything. The main groups:

- `platform:*`: bring up `tap-system` (gateway, certificates, registry, egress proxy, console)
- `images:push`: build and pin the harness, runner, proxy and console images
- `agent:*`: the agent lifecycle (`new`, `validate`, `diff`, `test`, `push`, `deploy`, `dev`, `logs`, `destroy`, `mcp-*`)
- `isolation:test`: deploy `probe-agent` and check that tools can't read secrets or reach undeclared networks
- `docs:cli`: regenerate `docs/cli.md`

## Repository layout

```
agents/          one directory per agent bundle
cmd/             tapctl, harness, runner, egress-proxy, console
pkg/             spec (schema + rules), bundle, render, runner, harness, egress, mcp, inventory, scaffold, version
schema/          agent.v1.json, fixture.v1.json
images/          Dockerfiles for the curated images
deploy/platform  tap-system manifests (kustomize)
platform.yaml    registry, domain, model routes, pinned image digests
```
