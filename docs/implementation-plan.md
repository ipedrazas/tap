# tap: Implementation Plan

Oct 8, 2026 · derived from *Agent-as-OCI + Harness Design*

## Scope of this plan

Prove that we can **author several different agents as bundles and run them on the existing k3s cluster** under the design's isolation model. Out of scope for now: the factory agent (pi-durable), the signing/attestation pipeline, Kyverno admission, OpenBao/ESO. Each is stubbed so it slots in later without reshaping what we build now.

Deliverables:

1. `agent.yaml` v1 JSON Schema, **extended with remote MCP servers**.
2. Go **runner** (`/v1/call` over loopback, see correction 6) and Go **harness** (model loop + socket client + HTTP chat API).
3. `tapctl` Go CLI: validate, permission diff, bundle build, render Kubernetes manifests.
4. A Claude Code **skill** (`.claude/skills/new-agent`) that generates spec-conformant bundles. The factory agent will later reuse the same instructions and templates.
5. `Taskfile.yml` driving everything.
6. Three reference agents deployed at `https://<agent>.a.hiddenfield.dev`.

## What I found in the cluster

| Item | State | Consequence |
| --- | --- | --- |
| k3s v1.36.4, containerd 2.3.4, 3 nodes (4 CPU / 8 GiB each) | ok | |
| `ImageVolume` feature gate | **enabled** | bundle mounted as an image volume; no init-container fallback needed in v1 |
| `SidecarContainers` | enabled | runner as native sidecar works |
| RuntimeClass `gvisor` (handler `runsc`) | present | |
| cert-manager `ClusterIssuer/letsencrypt` | DNS-01 via Cloudflare | wildcard `*.a.hiddenfield.dev` certificate is possible |
| Envoy Gateway, `Gateway/kodo` at 192.168.2.224 | listeners for `*.hiddenfield.dev` and `*.g.hiddenfield.dev` | `tap.hiddenfield.dev` already matches; **`*.a.hiddenfield.dev` does not** (wildcards cover one label), so a listener plus certificate is needed |
| Envoy AI Gateway in `kodo-inference` (backends `openrouter`, `sim`) | present | the harness can call models through it; `sim` gives cheap smoke tests |
| Dex at `auth.hiddenfield.dev` | present | OIDC `SecurityPolicy` in front of agent routes |
| In-cluster OCI registry | none, so zot was added | see decisions |
| Local tools | `task`, `kubectl`, `helm`, `go`, `docker`, `bun` | missing: `oras`, `crane`, `cosign` (add `task tools`) |

## Corrections to the design doc

These came up while mapping the design onto Kubernetes. I'd fold them into the doc.

1. **RuntimeClass is per pod, not per container.** You can't run the harness on runc and the runner on runsc in the same pod. **Plan:** run the whole agent pod under `gvisor`. The harness gets stronger isolation at a small syscall cost. The Unix-socket emptyDir works inside one sandbox.
2. **(Implemented in Phase 3.) NetworkPolicy is per pod, and the containers share a network namespace.** "Harness reaches only the model, runner only tool hosts" can't be enforced with NetworkPolicy alone. **Plan:** the pod may only reach DNS, the model endpoint, and a central **egress proxy** in `tap-system`. Each container authenticates to the proxy with its own credential: the harness credential allows the model host only; the runner mints a per-call credential that allows only that tool's `egress` list. This also covers the hostname-vs-IP open question.
3. **Image-volume artifact format.** containerd mounts *images*, and a bare `oras push` of a directory (custom media types) may not unpack. **Plan:** publish the bundle as a minimal OCI image, one tar layer, built with `crane append --oci-empty-base`. zot rejects Docker v2 manifests, so it must be OCI.
5. **Image-volume subPaths must be directories** (containerd: `only directory subpath is supported`). `agent.yaml` can't be mounted on its own. **Plan:** the source layout stays as in the doc, but `tapctl bundle build` projects it into per-container directories inside the image:
   ```
   /harness/{agent.yaml, system.md, skills/}
   /runner/{agent.yaml, tools/, mcp/, node_modules/}
   ```
   Each container mounts only its own subPath. `tools/` is never visible to the harness, as the doc requires.
6. **Unix socket between containers doesn't work under gVisor here.** A shared emptyDir is a 9p gofer mount per container, and the socket created by the runner is invisible to the harness. gVisor fixes this with the `dev.gvisor.spec.mount.<vol>.share: pod` annotations, but this k3s containerd doesn't forward them to runsc. That needs `pod_annotations = ["dev.gvisor.*"]` in each node's containerd runtime config. **Plan for v1:** the runner listens on `127.0.0.1:7070` (verified working; not reachable from the pod IP) and requires a per-pod bearer token mounted into both containers. Switch back to a socket if the node config is changed. Known gap in both designs: tool processes share the runner's UID, so they can reach the runner endpoint. That's acceptable because they can only invoke the same agent's declared tools.
4. **Dev environment.** The doc says k3d plus a local registry. We use the real k3s cluster with a namespace prefix instead. `make dev` becomes `task agent:dev`.

## Spec extension: remote MCP servers

Remote MCP tools go through the **runner**, never the harness. The runner already holds secrets and egress, so the isolation model is unchanged. The runner is an MCP client (Streamable HTTP). The harness sees MCP tools as ordinary tools named `<server>__<tool>`.

```yaml
mcp:
  - name: github                       # ^[a-z][a-z0-9_]{2,31}$
    url: https://api.githubcopilot.com/mcp/
    transport: streamable-http         # only value in v1
    auth:
      type: bearer                     # bearer | header | none
      secret: GITHUB_TOKEN             # must be declared in top-level secrets
    tools:                             # explicit allowlist, no wildcards
      - name: get_issue
        effects: read
      - name: create_issue_comment
        effects: write
    timeout: 30s
    # egress is derived from url (api.githubcopilot.com:443); no extra declaration
```

Validation rules added to the existing seven:

8. Every MCP tool is listed explicitly with an `effects` level; tools the server exposes but the bundle doesn't list are never shown to the model.
9. `auth.secret` counts as a "use" for rule 3. The secret is attached only to that server's connection.
10. **Schema pinning:** `tapctl bundle build` connects once, snapshots the allowed tools' `inputSchema` into `mcp/<server>.tools.json` inside the bundle, and the runner refuses calls if the live schema has drifted. Without this the reviewed bundle could change behavior without changing digest.
11. Rule 7 (permission widening) covers new servers, new MCP tools and higher MCP `effects` levels.
12. Rule 5 (fixtures): MCP tools need a fixture that runs against a recorded response (`tests/mcp/<server>/<tool>.yaml`), not the live server.

`/v1/call` is unchanged: `{"tool": "github__get_issue", "args": {...}}`. The runner dispatches to the script or MCP backend.

## Repository layout

```
tap/
├── Taskfile.yml
├── go.work
├── schema/agent.v1.json              # JSON Schema (source of truth, embedded in tapctl/harness/runner)
├── platform.yaml                     # registry, domain, gateway, model routes, curated runner/harness digests
├── pkg/spec/                         # Go types + loader + validation rules 0–12 + permission diff
├── pkg/bundle/                       # reproducible bundle image build + push
├── pkg/render/                       # manifest templates (embedded) + golden tests
├── cmd/
│   ├── tapctl/                       # validate | diff | bundle build | render | push
│   ├── harness/                      # model loop, skills loader, socket client, HTTP API
│   ├── runner/                       # /v1/call server, argv substitution, MCP client, CONNECT credentials
│   └── egress-proxy/                 # HTTP CONNECT proxy with per-credential allowlists
├── images/
│   ├── harness/Dockerfile            # distroless static
│   ├── runner-node/Dockerfile        # node:22 + runner binary
│   ├── runner-python/Dockerfile      # python:3.13 + runner binary
│   └── runner-base/Dockerfile        # jq, curl, coreutils + runner binary
├── deploy/
│   ├── platform/                     # kustomize: tap-system ns, registry, egress-proxy, gateway listener, cert, SecurityPolicy
│   └── spikes/                       # Phase 0 experiments
├── agents/                           # one directory per bundle
│   ├── echo-agent/
│   ├── weather-agent/
│   └── github-agent/
└── .claude/skills/new-agent/
    ├── SKILL.md
    ├── reference/spec.md             # condensed agent.yaml rules for the model
    ├── reference/patterns.md         # tool-script patterns (TS, Python), JSON-on-stdout contract
    └── templates/                    # agent.yaml, system.md, SKILL.md, tool.ts, tool.py, fixture.yaml
```

## Kubernetes shape per agent

`tapctl render agents/<name>` produces the following (applied with `kubectl apply -k`; later this output is the GitOps manifest):

- `Namespace agent-<name>` (labels: `tap.hiddenfield.dev/agent`, pod-security `restricted`)
- `ServiceAccount` with `automountServiceAccountToken: false`
- `Secret`s: `model-credentials` (harness only), `tool-secrets` (runner only), `egress-creds` (one key per container). Created by `task secrets:*` from a local `.env.<agent>` for now; ESO later.
- `NetworkPolicy`: default-deny ingress/egress; egress to kube-dns, `tap-system/egress-proxy`, `kodo-inference` gateway; ingress from `envoy-gateway-system` only.
- `Deployment` (1 replica, `runtimeClassName: gvisor`)
  - volume `bundle`: `image.reference: <registry>/agents/<name>@sha256:…`, `pullPolicy: IfNotPresent`
  - volume `workspace`: emptyDir `sizeLimit` from `workspace.size`
  - Secret `runner-token` mounted into both containers (loopback auth)
  - initContainer `runner` (`restartPolicy: Always`): mounts `bundle` subPath `runner` at `/bundle`; startup probe = exec health check on `127.0.0.1:7070`
  - container `harness`: mounts `bundle` subPath `harness` at `/bundle`
  - security baseline from the doc on both; resources from `runner.resources` and harness defaults
- `Service` and `HTTPRoute` `<name>.a.hiddenfield.dev` → harness `:8080`
- `SecurityPolicy` (OIDC via Dex) on the route

Platform (`deploy/platform`, applied once):
- `tap-system` namespace: registry (see decisions), egress-proxy, `tap.hiddenfield.dev` landing/API placeholder
- `Certificate` `*.a.hiddenfield.dev` + `tap.hiddenfield.dev` (DNS-01)
- `Gateway/tap` with a listener for `*.a.hiddenfield.dev` (see decisions)

## Harness (v1, minimal)

- Loads `agent.yaml`, checks `harness.api == v1`, reads `system.md`.
- Skills: puts each skill's `SKILL.md` frontmatter (name, description) in the system prompt. A built-in `read_skill_file` tool reads paths under `skills/` only (progressive disclosure, read-only).
- Tools: builds tool definitions from `agent.yaml` (script tools plus pinned MCP tools) and forwards every call to the runner socket. It never executes anything itself.
- Model: an OpenAI-compatible client pointed at the `kodo-inference` AI gateway (`openrouter` for real models, `sim` for tests). The `harness.model` block maps to the gateway's model header. Native Anthropic API support can be added behind the same interface.
- `effects: write|irreversible`: a v1 policy flag `approval: auto|deny`. Interactive approval comes later.
- API: `POST /v1/chat` (SSE stream, session id), `GET /healthz`. Sessions live in memory plus `/workspace/sessions/` (the long-task question stays open).
- Audit: structured JSON logs to stdout.

## Runner (v1)

Implements the doc's six per-call steps exactly, plus:
- per-call egress credential → `HTTPS_PROXY=http://<cred>@egress-proxy.tap-system:3128` in the child env (and nothing else beyond declared secrets, `PATH`, `HOME=/workspace/.home/<tool>`)
- process group kill on timeout, 1 MiB stdout cap, stderr to logs
- `exec[0]` checked against an interpreter allowlist baked into each runner image (`/etc/tap/interpreters`)
- MCP backend: one session per server, lazy connect, schema drift check against `mcp/<server>.tools.json`

## Phases (tracer bullets: each ends with something running on the cluster)

### Phase 0: Platform and spikes
- [x] `tap-system`: `Gateway/tap` (192.168.2.225), cert for `tap`, `registry`, `*.a` (DNS-01), zot, landing page, OIDC `SecurityPolicy` (`task platform:up`)
- [x] Image volume under gVisor: works read-only; directory subPaths only (correction 5)
- [x] Sidecar ↔ main container: Unix socket fails, loopback TCP works (correction 6)
- [x] Bundle pushed to zot as an OCI image (`crane append --oci-empty-base`)
- [x] DNS for `tap`, `registry`, `*.a` → 192.168.2.225 (Pi-hole at 192.168.2.53 needed `server=/hiddenfield.dev/1.1.1.1` and `rebind-domain-ok=/hiddenfield.dev/`)
- [x] Register the `tap` client in Dex (`task auth:dex`, then mirror the change in `kodo/deploy/k3s/dex.yaml`)
- [x] Bundle pulled from `registry.hiddenfield.dev` by node containerd and mounted (`task spike:bundle`)
- [x] Finding: bundle tars must be built deterministically (macOS tar leaks `._*` files, uid 501 and mtimes). `tapctl bundle build` writes the tar in Go: uid/gid 0, fixed mtime, sorted entries
- [x] Browser login through Dex at `https://tap.hiddenfield.dev`

### Phase 1: Spec and tooling
- [x] `schema/agent.v1.json` (incl. `mcp`, `effectsPolicy`) and `schema/fixture.v1.json`
- [x] `pkg/spec`: loader plus rules 0–6 and 8–12; `Diff` for rules 7 and 11 (base from `git:<ref>`, `oci:<ref>` or a file). The curated runner list and model routes live in `platform.yaml`
- [x] `pkg/bundle`: reproducible projection into `harness/` and `runner/`, uncompressed OCI layer, immutable version tags (`:dev` moves)
- [x] `pkg/render`: Namespace, ServiceAccount, 3 NetworkPolicies, Deployment (gVisor, image volume, native-sidecar runner), Service, HTTPRoute. Golden test; server-side dry run passes Pod Security `restricted`
- [x] `tapctl validate | diff | bundle build | render | secrets`; Taskfile `agent:*` tasks
- [x] `agents/echo-agent` reference bundle (fails rule 6 until the runner images are pushed in Phase 2)

Decisions made while implementing (stricter than the doc):
- **Rule 1** also rejects `env`, `xargs` and `busybox` as `exec[0]`, plus inline-code or module flags (`-c`, `-e`, `--eval`, `-p`, `-m`, `-r`, `--require`, `--import`, `--loader`, `-i`). For `node`/`python3`, `exec[1]` must be an existing script under `tools/`.
- **Rule 2** also requires every templated property to:
  - be a scalar;
  - if it's a string, carry a `pattern`, `enum`, `const` or `format`, so a value can't turn into a flag (argument injection);
  - be required or have a default.
  It also rejects properties that are never substituted into `exec`.
- **Rule 4** also rejects IP literals.
- **Rule 5** fixtures are `tests/<tool>.test.yaml` (`tool`, `cases[]` with `args`, optional stub `secrets`, recorded `http`, `expect.ok`, `error_kind` and `output_schema`). Fixture args must match `input_schema` unless the case expects `invalid_args`.
- `harness.model.provider` is `gateway` only for now; `name` must be one of the AI gateway routes in `platform.yaml` (`default`, `agent`, `sim`).
- `effectsPolicy: {write, irreversible}` is required; each is `auto`, `deny` or `ask`. Loosening it counts as widening in the diff.

### Phase 2: Runner + first agent end-to-end
- [x] `tap-runner` (`pkg/runner`, `cmd/runner`): `/v1/call` on loopback with a bearer token, schema validation, whole-slot argv substitution (and a refusal of string values starting with `-`), clean env with only declared secrets, timeout plus process-group kill, 1 MiB stdout cap, JSON-only stdout, stderr to logs, audit events. `tap-runner test` runs fixtures
- [x] `tap-harness` (`pkg/harness`, `cmd/harness`): OpenAI-compatible loop against the AI gateway (retries while the network policy admits a new pod), skills index plus the `read_skill_file` built-in, `effectsPolicy` enforcement, per-user sessions (identity from the forwarded Dex ID token) persisted in `/workspace/sessions`, SSE `/v1/chat`, built-in web UI at `/`
- [x] Images `harness` (distroless), `runner-node` (Node 22.23), `runner-python` (3.13.16), `runner-base` (jq, curl). Package managers removed; `/etc/tap/interpreters` drives rule 1 at runtime. `task images:push` builds OCI images and pins digests into `platform.yaml`
- [x] `task agent:test` runs fixtures as a Job in `tap-ci` (gVisor, real runner image, default-deny network); `task agent:test:local` for the fast loop. Bundles now carry `tests/` at the image root (mounted only by the test Job), so the digest covers the fixtures
- [x] `echo-agent` live at `https://echo-agent.a.hiddenfield.dev` behind Dex. Verified: model → tool calls → runner → answer; audit events; runner cannot see the model key or skills

Findings:
- **kodo-inference is locked down twice.** A NetworkPolicy admits only `kodo-gatekeeper`, and a `SecurityPolicy` requires an API key (`x-kodo-gateway-key`). Fixes: an additive policy `envoy-gateway-system/tap-agents-to-inference`, and `task model:client`, which registers a `tap` key. The key is copied into each agent namespace as `model-credentials` and mounted into the harness only.
- **kube-router takes a few seconds to admit a new pod's IP** into policy sets, so a pod's first connections are refused. The harness retries model calls; probes are unaffected.
- **`kubectl port-forward` doesn't work for gVisor pods** (separate network stack). To debug, exec into the runner and call `127.0.0.1:8080`.
- **Bug caught by tests:** embedding `bytes.Buffer` in the stdout cap promoted `ReadFrom`, so `io.Copy` bypassed the cap. A regression test now guards against it.

### Phase 3: Isolation and egress
- [x] `tap-egress-proxy` (`pkg/egress`, `cmd/egress-proxy`): CONNECT-only proxy in `tap-system`.
  - The runner mints a per-call token (HMAC with a per-agent key) naming agent and tool. The proxy allows only the hosts that tool declares.
  - The allowlist comes from the proxy's own policy (rendered from `agent.yaml`), not the token, so a leaked key can't widen an agent's reach.
  - It refuses destinations that resolve to private, loopback or link-local addresses.
  - It reads keys and policy straight from the API (a Role limited to two named objects), because mounted volumes lag a minute behind a patch and new agents failed their first calls.
- [x] **Privilege-separated runner** (decision Oct 8).
  - The runner runs as uid 0 inside gVisor with only `SETUID`, `SETGID`, `CHOWN` and `KILL`. Tool *i* runs as uid/gid `61000+i` with no supplementary groups.
  - Secret files are `0440 root:fsGroup`, so tools can't read the runner token, the egress key or other tools' secrets, nor each other's `HOME`.
  - Agent namespaces enforce Pod Security `baseline` (with a `restricted` warning); everything except the runner's capabilities meets `restricted`.
- [x] NetworkPolicies: agent pods reach DNS, the model gateway and (when they declare egress) the proxy, nothing else. The proxy reaches DNS, the API server and public addresses only.
- [x] Fixture mock: `tap-runner test` starts a TLS-intercepting mock proxy with a throwaway CA. Cases with recorded `http` run fully offline, undeclared hosts are refused, and an unrecorded request fails the case.
- [x] `agents/weather-agent` (Python, Open-Meteo). Live answers verified; each tool reaches only its own host.
- [x] `agents/probe-agent` plus `task isolation:test`: 17 checks, all passing.
  - Tools can't read the token, tool secrets, egress key, model key, PID 1's environment or other tools' homes.
  - Each tool runs non-root under its own uid, and only the declaring tool gets its secret.
  - Through the proxy, the declared host works; undeclared hosts and calls without a credential are refused.
  - Direct internet and the kube API are blocked; the model gateway answers 401 without the key.

Findings:
- **Node's env-proxy support (`NODE_USE_ENV_PROXY=1`) routes `http.request` too**, including a hand-built CONNECT, and attaches the tool's credential. The first version of the "no credential" probe was wrong for this reason; it now uses a raw socket.
- **Schema `pattern`s are compiled with Go RE2**, so ECMA escapes like `\u00C0` are rejected; use `\x{00C0}`. The skill needs to know this.
- **macOS Local Network privacy:** each rebuilt `tapctl` binary needs permission to reach 192.168.x.x. Accept the prompt once per build.

Known gaps (accepted for v1):
- **DNS:** pods can resolve arbitrary names through kube-dns, so DNS exfiltration is possible.
- **Model gateway:** tools can reach it at the network level; it's protected by the API key, which only the harness holds.
- **Cleartext HTTP:** the proxy supports CONNECT only, so tools can't use plain-HTTP APIs.

### Phase 4: The `new-agent` skill
- [x] `tapctl new` / `task agent:new`: deterministic scaffold for runner-node and runner-python. It validates as-is and pins the current runner digest, so a generated agent never starts from a hand-typed digest.
- [x] `.claude/skills/new-agent/`: `SKILL.md` (spec → scaffold → write → validate → test locally → test in cluster → report, never deploy unasked) plus `reference/spec.md`, `reference/tools.md` and `reference/troubleshooting.md`, covering every rule and gotcha found in phases 1–3.
- [x] **Tested the way the factory will use it.** Three fresh subagents, each given only the skill and a one-paragraph spec, ran in parallel:

  | Agent | Runner | API | Gates | Deployed |
  | --- | --- | --- | --- | --- |
  | `exchange-agent` | node | Frankfurter | validate ✓ · local 14/14 · cluster 14/14 | ✓ live answers |
  | `hn-agent` | python | HN Firebase | validate ✓ · local 12/12 · cluster 12/12 | ✓ live answers; 31-request fan-out in 1.3 s |
  | `countries-agent` | node | REST Countries | validate ✓ · local 14/14 · cluster 14/14 | ✓ live answers once a v5 key was in `.env.countries-agent` (v3.1 in the spec is retired; the subagent flagged it and adapted) |

  All three passed every gate on the first attempt, with no tooling failures.
- [x] Friction logs folded back into the skill:
  - Declared hosts: use the final host after redirects. If the API no longer matches the spec, stop and report it rather than quietly widening permissions. A **Decisions for the user** section is required in the report, and unattended runs must choose conservatively.
  - Fixtures: never build URLs from the local clock. List which recordings are trimmed and which are hand-built. Recordings are matched in any order and can be reused. `body: null` is JSON null. Draft 2020-12.
  - Tools: helper modules under `tools/` are allowed. The runner's argv formatting for numbers and its handling of defaults are now documented, as are Unicode classes in RE2, `URLSearchParams` encoding and the request budget for fan-out.
  - Tooling: `agent:diff` prints `effectsPolicy`, `agent:test` no longer echoes its script, the scaffold prompt reads properly, and `agent:test:local VERBOSE=1` shows tool stderr.
- These friction logs are the seed of the factory's eval set: the same specs, rerun against new skill versions.

### Phase 5: Remote MCP
- [x] `pkg/mcp`: minimal Streamable HTTP client (initialize, tools/list with pagination, tools/call). It handles JSON and SSE responses, sessions, and re-initialization on expiry. `mcptest` is a fake server for tests.
- [x] **Runner as MCP client.**
  - It connects through the egress proxy with an `mcp:<server>` credential minted per tunnel, and injects the declared auth secret (`bearer` or `header`).
  - It validates args against the pinned `inputSchema` and refuses tools that aren't allowlisted.
  - It re-checks the live schemas every 10 minutes and **refuses any drifted tool** until it's re-snapshotted, reviewed and redeployed.
  - Results are shaped for the model (`structuredContent`, `{"text"}` or `content`); `isError` becomes `upstream`. The 1 MiB cap and the per-server timeout apply. MCP calls write the same audit events as script tools.
- [x] `tapctl mcp list | snapshot [--check] | call`, plus Taskfile `agent:mcp-*`.
  - `list` shows everything a server offers, with its annotations.
  - `snapshot` pins the allowlisted schemas and warns when annotations contradict `effects: read`.
  - `call` records real `mcp_response` bodies for fixtures. MCP fixtures run offline against a stub that never drifts.
- [x] Snapshot format unified as `{tool: {description, inputSchema}}` across validator, harness and runner. The model-visible name must fit 64 characters, and MCP tool names are limited to `[A-Za-z0-9_-]`.
- [x] Skill: `reference/mcp.md` (declaring, `mcp-list` → allowlist → snapshot → review → record → fixtures, and the output shape). Generated by a fresh subagent from a one-paragraph spec:
  - **`repo-agent`** (MCP-only, DeepWiki): validate ✓, local 7/7, cluster 7/7, deployed. It explained go-task/task using DeepWiki through the proxy.
  - It allowlisted 2 of 3 tools and left out `read_wiki_contents` (140–635 KB per call).

Findings:
- **The runner had no CA roots in the node image.** `node:*-slim` ships no `ca-certificates`: Node tools worked because Node bundles its own roots, but the runner's own TLS (MCP) failed certificate verification. Fixed by embedding Go's fallback root store in `tap-runner`. Stub-backed fixtures couldn't catch this; only the live call did.
- From the subagent's friction log, all fixed:
  - `tapctl new` wrote the description unquoted, so a `: ` broke the YAML. Scaffold values are now encoded as JSON strings.
  - There was no way to see the tools that aren't allowlisted. Added `mcp list`, and `snapshot` now prints the skipped names.
  - The `mcp.md` example used the wrong DeepWiki tool name and assumed `{"text"}` output where DeepWiki returns `structuredContent`.
  - The doc didn't say the output cap applies to MCP results, or how to clean up an MCP-only agent.
  - `agent:test` pushing a `:dev` bundle is now called out as part of testing, not deploying.

### Phase 6: Inventory, console and CLI docs
- [x] `tapctl render` stamps each agent Deployment with inventory annotations: description, owner, model, tools, MCP servers, egress hosts and URL. Values are JSON-quoted so any text is valid YAML.
- [x] `pkg/inventory`: lists agents from Deployments and Pods across namespaces in two API calls. It reports status (Ready, Progressing, Degraded, Scaled down) and the reason a container is waiting.
- [x] `tapctl agent ls [-o table|wide|json]` and `tapctl agent get <name>`, using the standard kubeconfig rules (`--kubeconfig`, `--context`).
- [x] `tap-console` at https://tap.hiddenfield.dev, behind Dex, replacing the placeholder page.
  - It shares `pkg/inventory` with the CLI and shows a card per agent (status, version, model, tools, egress, bundle digest, pod problems).
  - It serves `/api/agents` and `/api/agents/<name>` as JSON.
  - Its ClusterRole allows only list/get on Deployments and Pods.
- [x] `pkg/version`: every binary reports version, commit, build date and dirty flag with `--version`, and logs its build at startup.
  - Release builds stamp the values via `-ldflags`: the Taskfile and Dockerfiles pass `git describe`, the commit and the UTC build time.
  - Plain `go build` falls back to Go's embedded VCS info.
- [x] CLI docs: `tapctl help`, grouped by task; `tapctl help <cmd>` and `<cmd> -h` show usage, explanation, examples and flags. `docs/cli.md` is generated from the same command table (`task docs:cli`), and a test fails if it's stale. A README covers the components, quickstart and layout.

## Roadmap (agreed Oct 9, in this order)

1. **Phase 7: Signing and admission.**
   - cosign-sign bundles and curated images at push time.
   - Add in-toto attestations: spec hash, fixture results, permission diff, builder identity.
   - Install the sigstore policy-controller or Kyverno, in audit mode first, then enforce: reject agent pods whose bundle or images aren't signed by the pipeline key.
2. **Phase 8: OpenBao + External Secrets.**
   - OpenBao 2.5.5 is reachable from the cluster at `http://openbao.alacasa.uk:8200` (192.168.2.131; plain HTTP, not TLS on 443).
   - Install External Secrets Operator with one auth role per agent namespace (Kubernetes auth), and map `secrets.<NAME>.from: vault://<path>` to ExternalSecrets.
   - Seed with `task secrets:put`, and retire `.env.<agent>` for deployed agents.
   - **Decision needed:** put TLS in front of OpenBao, or accept plain HTTP on the LAN.
3. **Phase 9: Factory agent** on `@earendil-works/pi-durable`. Spec → PR using the `new-agent` skill and `tapctl`, with no registry push rights and no cluster credentials. The subagent friction logs from phases 4–5 are its first eval set.
4. **Phase 10: Approvals** for `write` / `irreversible` tools. `effectsPolicy: ask` is already reserved: the harness pauses and the console (or Slack) approves.

## Taskfile (initial surface)

```
task tools                         # install oras, crane, cosign
task platform:up                   # apply deploy/platform (ns, registry, proxy, cert, listener)
task images:build images:push      # harness, runner-*, egress-proxy
task agent:new NAME=foo            # scaffold from skill templates (non-interactive)
task agent:validate AGENT=foo      # tapctl validate + diff
task agent:test AGENT=foo          # fixtures in runner image on gVisor
task agent:push AGENT=foo          # bundle build → push → writes digest to .tap/foo.digest
task agent:deploy AGENT=foo        # render with digest → kubectl apply
task agent:dev AGENT=foo           # validate + push + deploy + rollout status
task agent:logs AGENT=foo          # both containers
task agent:chat AGENT=foo -- "hi"  # curl the SSE endpoint
task agent:destroy AGENT=foo
```

## Decisions (resolved Oct 8)

| Topic | Decision |
| --- | --- |
| Registry | zot in `tap-system` at `registry.hiddenfield.dev`. Anonymous pull, `tap` user pushes |
| Gateway | dedicated `Gateway/tap` on 192.168.2.225 (MetalLB). Listeners: `tap`, `registry`, `agents` (`*.a.hiddenfield.dev`), `http` (redirect) |
| Models | via the `kodo-inference` AI gateway (OpenRouter, plus `sim` for tests) |
| Auth | Dex OIDC. One `SecurityPolicy` on the `tap` and `agents` listeners. Shared callback `https://tap.hiddenfield.dev/oauth2/callback`, cookie domain `hiddenfield.dev`. The registry listener is not behind OIDC |
