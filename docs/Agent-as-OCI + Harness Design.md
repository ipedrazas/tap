# Agent-as-OCI + Harness: Design

Oct 8, 2026 · @Ivan Pedrazas

## Context and goals

Every agent ships as a signed OCI bundle (prompts, skills, scripts, manifest) that runs on one generic Go harness image. A factory agent writes bundles; it never builds images or deploys.

The factory agent generates new agents from a spec: system prompt, skills and tools. Tools are scripts in interpreted languages (TypeScript, Python), not compiled Go plugins, so no build step sits between "the agent wrote a tool" and "the tool runs".

**Goals**

- **Isolation:** an agent cannot read another agent's files, secrets or processes, and a tool cannot read the agent's model key.
- **Immutability:** what runs in production is exactly what was reviewed, addressed by digest; an agent cannot edit its own prompt, skills or tools at runtime.
- **Least privilege by declaration:** every secret, egress domain and tool an agent uses is declared in its manifest and enforced by the platform.
- **Provenance:** for any running agent we can say which factory run, spec and tests produced it.
- **Independent release cycles:** the harness upgrades without rebuilding agents; agents ship without touching the harness.

**Non-goals (v1)**

- Agents that modify or spawn other agents at runtime.
- Multi-tenant agents sharing one pod.
- Long-lived stateful tools (connection pools, websockets) inside the script runner; those graduate to MCP sidecars.

## Architecture overview

An agent is a generic harness image plus a bundle pulled by digest; the agent's identity lives entirely in the bundle.

&#91;embedded content: agent pod · harness, tool-runner, bundle, workspace\]

The harness only talks to the model and the runner. The runner alone reaches external APIs, and only the hosts each tool declares. The bundle is read-only to both containers, so an agent cannot rewrite its own prompt, skills or tools.

## Bundle layout

A bundle is a directory of data, never executables the harness loads. It is pushed as an OCI artifact (`oras push`) to `registry/agents/<name>:<semver>` and always referenced by digest in production.

```
agents/invoice-agent/
├── agent.yaml            # the contract: identity, harness API, tools, secrets, egress
├── system.md             # system prompt
├── skills/
│   └── billing/
│       ├── SKILL.md      # when and why to use the tools
│       └── reference.md  # progressive-disclosure material
├── tools/
│   ├── fetch_invoice.ts
│   └── lib/              # shared helpers, vendored
├── tests/
│   └── fetch_invoice.test.yaml   # fixture: args in, expected output shape out
└── package-lock.json     # pinned deps, installed at build into the bundle
```

| Path | Read by | Rules |
| --- | --- | --- |
| `agent.yaml` | harness, tool-runner, pipeline | required; schema-validated |
| `system.md` | harness | required; plain markdown |
| `skills/` | harness | loaded progressively, SKILL.md frontmatter first |
| `tools/` | tool-runner only | never mounted into the harness container |
| `tests/` | pipeline | one fixture per tool minimum |
| lockfile | pipeline | dependencies are vendored at build; no install at runtime |

Dependencies are installed by the pipeline and vendored into the bundle (`node_modules/` or a site-packages dir), so the runner never reaches a package registry at runtime.

## agent.yaml spec

`agent.yaml` is the single source of truth for what an agent may do; the harness, the tool-runner and the pipeline all enforce it, and anything undeclared is denied.

```yaml
apiVersion: tavon.ai/agent/v1
kind: Agent
metadata:
  name: invoice-agent
  version: 1.3.0
  owner: team-billing
  description: Answers questions about customer invoices

harness:
  api: v1                     # harness refuses bundles it does not support
  model:
    provider: anthropic
    name: claude-sonnet-5-5
  maxTurns: 40

prompt: system.md
skills:
  - skills/billing

runner:
  image: runner-node@sha256:…   # from the curated set only
  timeout: 30s                  # default per tool call
  resources: { cpu: 500m, memory: 256Mi }

tools:
  - name: fetch_invoice
    description: Fetch an invoice PDF by ID from the billing API
    input_schema:
      type: object
      properties:
        invoice_id: { type: string, pattern: "^INV-[0-9]{6}$" }
      required: [invoice_id]
      additionalProperties: false
    exec: ["node", "tools/fetch_invoice.ts", "--id", "{{invoice_id}}"]
    timeout: 20s
    secrets: [BILLING_API_TOKEN]
    egress: [api.billing.example.com:443]
    effects: read             # read | write | irreversible

secrets:
  BILLING_API_TOKEN:
    from: vault://billing/readonly-token

workspace:
  size: 1Gi                   # emptyDir shared by harness and runner
```

### Tool fields

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | unique within the agent; `^[a-z][a-z0-9_]{2,63}$` |
| `description` | yes | shown to the model; one sentence |
| `input_schema` | yes | JSON Schema; `additionalProperties: false` required |
| `exec` | yes | argv array; `{{arg}}` substitutes one validated value into one argv slot |
| `timeout` | no | overrides `runner.timeout` |
| `secrets` | no | names from the top-level `secrets` map; injected only for this call |
| `egress` | no | `host:port` allowlist; empty means no network |
| `effects` | yes | `write` and `irreversible` can require human approval at runtime |

### Validation rules (pipeline-enforced)

1. `exec[0]` must be an interpreter present in the selected runner image; no `sh`, `bash` or `-c`.
2. Every `{{arg}}` maps to a property in `input_schema`, and substitution fills a whole argv element, never part of one.
3. Every secret a tool lists is declared in `secrets`, and every declared secret is used by at least one tool.
4. No wildcard egress (`*`, `*.com`); subdomain wildcards need an explicit exception.
5. Every tool has at least one fixture in `tests/`.
6. `runner.image` is pinned by digest and on the curated list.
7. A version bump that widens permissions (new secret, egress, tool or effect level) is flagged for human review with a diff.

## Runtime

One pod per agent, one namespace per agent, two containers with separate identities: the harness thinks, the tool-runner acts, and neither holds the other's secrets.

### Containers

|  | Harness | Tool-runner |
| --- | --- | --- |
| Image | generic `harness@sha256:…` | curated `runner-<lang>@sha256:…` |
| Kind | main container | native sidecar (init container, `restartPolicy: Always`) |
| Runtime class | runc | gVisor (`runsc`) |
| Bundle mount | `system.md`, `skills/`, `agent.yaml` (read-only) | `tools/`, `agent.yaml` (read-only) |
| Secrets | model API key only | per-tool secrets from `agent.yaml` |
| Egress | model provider only | union of tool `egress` lists, enforced per call |
| Workspace | `/workspace` (rw emptyDir) | `/workspace` (rw emptyDir) |

The bundle is mounted with a Kubernetes image volume (`volumes[].image.reference: registry/agents/invoice-agent@sha256:…`), so the harness image never changes per agent. Where image volumes are unavailable, an init container copies the bundle into a read-only emptyDir.

### Harness to runner protocol

The harness calls the runner over a Unix socket on a dedicated emptyDir, not over the network.

```
POST /v1/call
{ "tool": "fetch_invoice", "args": { "invoice_id": "INV-004211" }, "call_id": "c-81" }

200 { "call_id": "c-81", "ok": true, "output": { … }, "duration_ms": 412 }
200 { "call_id": "c-81", "ok": false, "error": { "kind": "timeout", "message": "…" } }
```

The runner, for every call:

1. Rejects unknown tools and validates `args` against `input_schema`.
2. Builds argv by slot substitution; never invokes a shell.
3. Spawns the process with a clean environment holding only that tool's secrets.
4. Applies the tool timeout and output size cap (default 1 MiB), then kills the process group.
5. Requires JSON on stdout; stderr goes to logs, never to the model.
6. Emits an audit event: agent, tool, args hash, duration, exit status.

For `effects: write | irreversible`, the harness can pause and request human approval before calling the runner; policy decides per agent.

### Curated runner images

- `runner-node` (Node 22 with native TypeScript stripping), `runner-python` (3.13), `runner-base` (jq, curl, coreutils).
- Rebuilt weekly, scanned, signed; agents pin by digest and the pipeline bumps them.
- A custom runner image is an exception path with its own review.

### Security baseline (both containers)

- Non-root UID, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, all capabilities dropped, seccomp `RuntimeDefault`.
- CPU, memory and PID limits; `automountServiceAccountToken: false`.
- Default-deny NetworkPolicy per namespace; egress opened from `agent.yaml` only (an egress proxy for hostname rules, since NetworkPolicy matches IPs).
- No hostPath, no Docker socket, no volumes shared across agents.
- Secrets live in OpenBao/Vault and are synced by External Secrets Operator, with one policy and auth role per agent namespace.

## Lifecycle

The factory's authority ends at a pull request; the pipeline builds, tests and signs, a human approves permission changes, and the cluster admits only signed digests.

1. **Factory writes the bundle.** From a spec, it produces the bundle directory and opens a PR against the agents repo. It has no registry push rights, no cluster credentials and no production secrets.
2. **Pipeline validates.** Schema and the validation rules above; a permission diff against the previous version.
3. **Pipeline builds.** Installs pinned dependencies into the bundle, runs `oras push` to the staging registry, generates an SBOM.
4. **Pipeline tests in a sandbox.** Runs every tool fixture in the real runner image under gVisor with stub secrets and a mock egress proxy, then a short harness smoke conversation.
5. **Review.** Auto-approve when permissions are unchanged and tests pass; human approval when the diff widens permissions or adds `irreversible` effects.
6. **Sign and attest.** cosign signature plus in-toto attestations: factory run ID, spec hash, model, test results, permission diff.
7. **Promote.** Copy by digest to the production registry; update the agent's GitOps manifest to the new digest.
8. **Admit.** Kyverno or the sigstore policy-controller rejects any bundle or runner image not signed by the pipeline identity.

**Rollback** is a GitOps revert to the previous digest; no rebuild.

**Harness upgrades** ship on their own cadence. A new harness release declares the bundle API versions it supports, and the pipeline re-runs smoke tests for every deployed agent before rollout.

## Dev environment

Dev runs the same pods as production on a local k3d cluster, so there is one deployment model and no Docker socket anywhere.

- **Cluster:** k3d with the gVisor runtime class installed, a local registry (`k3d-registry.localhost:5000`) and the same admission policies in audit mode.
- **Inner loop:** `make dev AGENT=invoice-agent` pushes the bundle to the local registry and rolls the pod. A push takes seconds, so there is no need for a mount-based mode.
- **Factory locally:** runs outside the cluster with access to the agents repo checkout and the local registry only; it still produces bundles, never deploys.
- **Secrets:** a local OpenBao (dev mode) seeded with test values, matching OpenBao/Vault in prod, so the same `secrets.from` paths and External Secrets wiring work in both; production mounts and policies do not exist in dev.
- **Parity check:** the same `agent.yaml` and the same digests move from dev to staging unchanged.

## Open questions and next steps

**Open questions**

- Where should the harness run its LLM loop for long tasks: inside the pod, or should pods be ephemeral per task with state in the workspace?
- Do skills ever ship scripts the model is told to run directly (Anthropic Agent Skills style), or must every executable go through a declared tool? Proposed: declared tools only.
- How does a tool get structured input larger than argv allows (files, long text)? Proposed: write to `/workspace` and pass the path.
- Approval UX for `write` and `irreversible` effects: Slack, the harness UI, or both?
- Which image-volume support level do target clusters have, and is the init-container fallback needed for v1?
- Is an egress proxy (hostname allowlists) acceptable operationally, or is IP-based NetworkPolicy enough to start?

**Next steps**

- [ ] Freeze `agent.yaml` v1 schema as JSON Schema in the harness repo
- [ ] Implement the runner (`/v1/call`, argv substitution, timeouts, audit events)
- [ ] Split the current harness: remove tool execution, add the socket client
- [ ] Build `runner-node` and `runner-python` images and the signing pipeline
- [ ] k3d dev cluster with gVisor, local registry and admission policies
- [ ] Port one existing agent to a bundle end to end as the reference
- [ ] Point the factory agent at the bundle format and the PR-only workflow
