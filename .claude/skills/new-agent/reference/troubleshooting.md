# Failures and fixes

| Symptom | Cause | Fix |
| --- | --- | --- |
| `rule 2 ... never substituted into exec` | a schema property isn't used in argv | add `"--x", "{{x}}"` to `exec`, or remove the property |
| `rule 2 ... string without pattern, enum, const or format` | unconstrained templated string | add a `pattern` that excludes a leading `-` (e.g. `"^[^-][^\\n]{0,199}$"`) |
| `rule 2 ... must be required or have a default` | optional templated property | add to `required` or give it a `default` |
| `invalid JSON Schema ... invalid escape sequence` | ECMAScript regex syntax | rewrite for Go RE2 (`\x{00C0}`, no lookahead) |
| `rule 1 ... must be a script under tools/` | wrong `exec[1]` | `["node", "tools/<file>.ts", ...]` |
| `rule 3 ... declared but not used` | secret not listed on any tool | add it to the tool's `secrets`, or delete it |
| `rule 6 ... not a curated runner image` | hand-typed or stale digest | `bin/tapctl runner bump agents/<name>` |
| `rule 5 ... do not match input_schema` | fixture args violate the schema | fix the args, or expect `error_kind: invalid_args` |
| fixture `egress: no recording for GET https://...` | the tool's URL differs from the recording | copy the exact URL from the message into `http[].url` |
| fixture `egress: egress to X is not declared` | the tool called a host not in `egress` | add `X:443` to the tool's `egress` (and check why it calls X) |
| fixture `want ok=true, got exit` | the tool crashed | `task agent:test:local AGENT=<name> VERBOSE=1` shows each tool's stderr (the `"stderr"` field in the JSON log lines) |
| fixture `bad_output` | something other than one JSON value on stdout | remove `console.log`/`print` debugging; write JSON once |
| `agent:push` says the version already points at another digest | content changed without a version bump | bump `metadata.version` |
| `attest: agents/<name> builds to sha256:..., not ...` | sources changed after `agent:test` pushed | rerun `task agent:test`; the attestation must describe the pushed bytes |
| `.tap/cosign.pub is not in platform.yaml signing.*` / `no signing key` | signing keys missing or rotated | ask the user; never generate or replace keys yourself |
| admission: `the bundle has no passing fixture run signed by the pipeline` | deployed a bundle that never passed `agent:test` | run `task agent:test`, then deploy |
| admission: `is not a registry.hiddenfield.dev/tap/ image pinned by digest` | a hand-edited image or a tag | use the curated digests from `platform.yaml` (`tapctl runner bump`) |
| `tapctl: ... dial tcp 192.168.2.225:443: i/o timeout` on a Mac | macOS Local Network permission for a freshly built `tapctl` | ask the user to allow the prompt, then retry |

Running a tool by hand (from the repo root):
```bash
cd agents/<name> && node tools/<tool>.ts --arg value     # or python3 tools/<tool>.py ...
```
Without the runner there's no proxy and no clean environment, so this only debugs logic and parsing.
