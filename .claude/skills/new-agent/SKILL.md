---
name: new-agent
description: Create a new tap agent bundle (agent.yaml, system prompt, skills, tool scripts, fixtures) from a short spec, validate it, and test it in its real runner image. Use when asked to create, generate or scaffold an agent for the tap platform, or to add tools to an existing one.
---

# Create a tap agent

You turn a short spec ("an agent that answers X using API Y") into a bundle under `agents/<name>/` that passes `tapctl validate` and its fixtures, and you report what it is allowed to do. You never deploy unless the user explicitly asks; a bundle that is valid and tested is the deliverable.

Read `reference/spec.md` before writing `agent.yaml`, `reference/tools.md` before writing tool code, and `reference/mcp.md` before declaring an MCP server. They're short, and every rule in them is enforced.

## Workflow

### 1. Write the brief

Before any file, write down the agent's contract. Specs are written for people and leave things out; the brief is where the gaps show. For each item, note whether the spec says it, you checked it against the API, or you're assuming it.

- **Name**: DNS label ending in `-agent` (e.g. `exchange-agent`). **Runner**: `runner-node` (default; TypeScript) or `runner-python`. Tools may use the standard library only: no npm or pip packages.
- **Purpose**: one sentence; it becomes `metadata.description` (≤ 200 chars).
- **What people give it**: what someone types in the chat (a PR URL, a city, a question). Values, not documents: if someone would have to paste a diff, a file or logs, the agent should probably fetch it instead.
- **Tools**: for each, what it does, the host it calls, its effect level (`read`, `write`, `irreversible`), any API key, and **where each input comes from**: something the person types, another tool's output, or a constant. Nothing else supplies inputs (see "Designing tools" in `reference/tools.md`).
- **MCP servers**: if the spec names a remote MCP server (or a service offers one and that's simpler than writing tools), use it. Read `reference/mcp.md`.
- **Where results go**: the chat, unless the spec asks the agent to post or send something (that's `write`, and needs an explicit yes).
- **Two example exchanges**: what someone asks, which tools run, what the answer contains. A tool no example uses is probably not needed.

Ask the user about every gap you can't settle from the spec or the API: where an input comes from, a required API key's name, an ambiguous effect level, where results go. Don't fill a gap by inventing a tool around input nobody can provide.

The factory (`factory/`) runs this skill in two phases with an addendum (`factory/src/addendum.md`): it submits the brief with `submit_brief`, code checks it and asks the person about each gap, and only then is the agent built. The addendum also swaps the `task` commands for their direct equivalents and skips the in-cluster steps. When a build runs unattended, pick the most conservative option for anything still open and list each choice under **Decisions for the user** in your report.

Before designing tools, look at the real API: fetch its documentation and one real response per endpoint (with WebFetch or `curl -sSi` from your shell). You need the exact URL shape and response fields to write both the tool and its fixtures. While doing so:

- **Follow redirects and use the final host.** Call it directly and list only that host in `egress`. Undeclared redirect targets fail at runtime.
- **If the API no longer matches the spec** (retired, moved, now needs a key, paid only), don't silently change the agent's permissions. Report it under **Decisions for the user**. If you continue, adapt the design (for example, add the secret) and say exactly what changed and why.
- Note the API's limits (rate limits, page sizes). They shape how many requests a tool may make per call.

### 2. Scaffold

```bash
task agent:new NAME=<name> RUNNER=runner-node|runner-python DESCRIPTION="<one sentence>" OWNER=<team>
```

This writes a bundle that already validates, with the current pinned runner digest. Never type a runner digest yourself. `OWNER` defaults to `platform`. Delete the example tool's script and test once you've replaced it.

### 3. Write the bundle

Replace the example tool. For each tool, write in this order:

1. its entry in `agent.yaml` (schema, `exec`, `egress`, `secrets`, `effects`);
2. the script in `tools/`;
3. `tests/<tool>.test.yaml`, with at least a happy path, an expected-failure case (not found, upstream error) and an `invalid_args` case.
   - Record bodies from the real API. Trimming them to the fields the tool reads is fine, but keep the real shape.
   - If you can't capture a real response (a key is required, a demo key returns canned data, an error you can't trigger), you may hand-build it in the real shape. List which fixtures are hand-built in your report: they're unverified.

For each MCP server: declare it in `agent.yaml`, run `task agent:mcp-snapshot AGENT=<name>`, review the pinned descriptions and schemas, record fixtures with `task agent:mcp-call`, and write `tests/mcp/<server>/<tool>.test.yaml` (see `reference/mcp.md`).

Then write `system.md` (role, when to use which tool, how to answer) and the skill's `SKILL.md` (what each output field means). Keep the system prompt short and concrete.

### 4. Validate, then test

```bash
task agent:validate AGENT=<name>
task agent:test:local AGENT=<name>     # fast; uses host node/python3
task agent:test AGENT=<name>           # the real gate: runner image, gVisor, no network
```

`agent:test` pushes the bundle to the registry under the moving `:dev` tag and runs a Job in `tap-ci`. That is part of testing, not deploying: nothing serves traffic. When the fixtures pass it also signs the bundle as tested and attaches a signed attestation of the run; admission requires both, so a bundle that never passed `agent:test` cannot be deployed. `task agent:verify AGENT=<name>` shows what was recorded.

Fix every finding and failure and rerun until all three pass. Don't weaken a rule to get past it (e.g. dropping a `pattern`); fix the design. `reference/troubleshooting.md` maps common failures to fixes.

### 5. Report

Run `task agent:diff AGENT=<name>`. For a new agent this exits 3 and lists every permission; that's expected. Give the user:

- what the agent does and its tools (one line each);
- the permission summary: hosts per tool, secrets, effect levels, and `effectsPolicy`;
- the fixture results, and which recordings are hand-built rather than real;
- for any secrets: "store each one in OpenBao before deploying: `pbpaste | task secrets:put AGENT=<name> NAME=<NAME>`" (never ask for or handle the values yourself);
- **Decisions for the user**: every assumption or spec deviation, or "none".

`agent:diff` lists tools, effects, egress and secrets; `effectsPolicy` is printed on its first line. `task` prints `Failed ... exit status 3` for a new agent; that's the expected "review required" result, not an error.

Deploying is `task agent:dev AGENT=<name>`, or `task agent:launch AGENT=<name>` for an agent that has just merged (it also checks that main is current and the secrets are in OpenBao, and runs a smoke chat). Only run either when the user asks. After deploying, wait about 10 seconds before the first chat (new pods are briefly blocked by the network policy controller).

## Changing an existing agent

Bump `metadata.version` for every change to `agent.yaml` (semver: patch for fixes, minor for new tools). Pushes of an existing version with different content are refused. Run `task agent:diff AGENT=<name>`: exit 3 means the change widens permissions, so call out exactly what widened.
