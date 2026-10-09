# agent.yaml rules (enforced by `tapctl validate`)

The schema is `schema/agent.v1.json`; the scaffold shows every field. These are the rules that bite.

## Identity and harness
- `metadata.name`: `^[a-z][a-z0-9-]{1,38}[a-z0-9]$`. It becomes namespace `agent-<name>` and host `<name>.a.hiddenfield.dev`.
- `metadata.description` ≤ 200 chars. `metadata.version` is semver.
- `harness.model.provider: gateway`. `harness.model.name` is one of the routes in `platform.yaml` (`agent` for real use; `sim` is a canned simulator).
- `effectsPolicy.write` / `.irreversible`: `auto`, `deny` or `ask` (`ask` currently behaves like deny). Keep `deny` unless the user wants the agent to act without approval.

## Tools
- `name`: `^[a-z][a-z0-9_]{2,63}$`, no `__`, not `read_skill_file`. `description` ≤ 300 chars, written for the model.
- `input_schema`: `type: object`, `additionalProperties: false`, and `properties` (may be `{}`).
- `exec`: argv array. `exec[0]` is an interpreter in the runner (`node` for runner-node, `python3` for runner-python). `exec[1]` is a script under `tools/`. No shells, no `-e`/`-c`/`-m`/`-r`/`--eval`/`--import` and similar.
- `{{name}}` fills one whole argv element (`"--id", "{{id}}"`, never `"--id={{id}}"`).
- **Every property** in `input_schema` must appear as a `{{property}}` in `exec`, or validation fails ("never substituted").
- Every templated property must be required or have a `default`.
- Templated values must be scalars: string, integer, number or boolean. Pass structured or large input as a file under `/workspace` and its path as a string.
- **Templated strings need `pattern`, `enum`, `const` or `format`.** Patterns must not allow a leading `-`. The runner also refuses any string value starting with `-`; numbers may be negative.
- `integer`, `number` and `boolean` properties need no pattern. Bound them with `minimum`/`maximum`.
- How values become argv: strings verbatim; integers as digits; numbers in plain decimal, never exponent form (`1e21` becomes `1000000000000000000000`, `12.5` stays `12.5`); booleans as `true`/`false`.
- `default` values are applied by the runner, at runtime and in fixtures, when the model or a fixture omits the property.
- **Patterns are Go RE2**, not ECMAScript: no lookahead or backreferences. Unicode classes work (`\p{L}`), and code points are written `\x{00C0}`, not `\u00C0`. In YAML double-quoted strings, escape backslashes (`"^\\d+$"`).
- `egress`: list of exact `host:port`. No wildcards, no IPs. Each host the tool contacts, including redirect targets, must be listed. HTTPS only: the proxy supports CONNECT, so plain-HTTP APIs don't work.
- `secrets`: names (`^[A-Z][A-Z0-9_]{1,63}$`) declared in top-level `secrets`. The tool receives each as an env var of the same name, and only tools that list it get it.
- `effects`: `read` | `write` | `irreversible`. Anything that changes external state is at least `write`.
- `timeout`: ≤ `5m`; defaults to `runner.timeout`.

## Secrets
```yaml
secrets:
  OPENWEATHER_KEY:
    from: vault://<agent-name>/<key-name>
```
Every declared secret must be used by a tool, and `from` must be `vault://<this agent's name>/<key>`: OpenBao only lets an agent read its own path (`secret/tap/agent-<name>/<key>`, field `value`). Deployed agents get them through External Secrets; the user stores values with `task secrets:put`. Locally (`tapctl mcp call`), values come from `.env.<agent>` (`NAME=value` lines). Fixtures use stub values and need neither.

## Skills
- `skills: [skills/<dir>]`. Each dir has `SKILL.md` with YAML frontmatter `name` and `description`. The harness lists them in the system prompt and the model reads files with `read_skill_file`.

## Fixtures (`tests/<tool>.test.yaml`)
```yaml
tool: <tool name>
cases:
  - name: <what it checks>
    args: { ... }                 # must match input_schema unless expecting invalid_args
    secrets: { API_KEY: stub }    # stub values for declared secrets
    http:                         # recorded responses; nothing else is reachable
      - method: GET
        url: "https://host/path?exact=query"   # must equal the URL the tool builds, byte for byte
        status: 200
        headers: { content-type: application/json }   # optional
        body: { ... }             # JSON value; a YAML string is sent as text/plain; `body: null` is JSON null
    expect:
      ok: true                    # false for runner-level failures
      error_kind: invalid_args    # with ok: false: invalid_args|timeout|exit|bad_output|output_too_large|denied
      output_schema: { ... }      # JSON Schema the tool's JSON output must satisfy
```
- One fixture file minimum per tool. An unrecorded request, or a request to a host not in the tool's `egress`, fails the case.
- Recordings are matched by method and exact URL, in any order, and can be matched more than once. Parallel requests are fine.
- A tool that makes N requests per call needs N recordings per case, so keep fixture inputs small (e.g. `limit: 3`).
- `output_schema` is JSON Schema draft 2020-12.
- **Never build URLs from the local clock** (today's date, "last 30 days"): the fixture would break tomorrow. Derive dates from upstream data (e.g. the API's latest date) or take them as arguments.

## YAML traps
- Unquoted `y`, `n`, `yes`, `no`, `on`, `off` are booleans in YAML 1.1. Don't use them as property names, and quote them as values.
- Quote strings containing `: `, `#`, `{`, `[`, or a leading `*`, `&`, `!`, `%`, `@`.
