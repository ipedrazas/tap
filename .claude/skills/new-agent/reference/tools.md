# Writing tool scripts

A tool is a script the runner starts with argv built from `exec`. It runs in a clean environment as its own unprivileged user, inside gVisor, with no network except the egress proxy.

## Designing tools

Decide what each tool takes before writing it. An agent gets exactly two things: what a person types in the chat, and what its own tools return.

- **The agent fetches its own data.** A PR reviewer takes a PR URL (or owner, repo and number) and a tool fetches the metadata and the diff from the GitHub API. It doesn't take the diff as input.
- **Every input needs a source someone actually has**: the person's message, another tool's output, or a constant. There is no invocation layer. Nobody fills `{PLACEHOLDERS}` in `system.md`, writes files to `/workspace` for the agent, or calls it with extra context. A tool that reads a file only another tool could have written is fine; a tool that reads a file nobody writes is dead.
- **Inputs are short values**: an id, a URL, a name, a date, a small enum. They travel through argv and must have a `pattern`, `enum` or `format` (rule 2), so a document can't be an input anyway.
- **A pasted system prompt isn't a design.** Persona, review rules and output format belong in `system.md`. The tools still come from: what does the person give, what must be fetched, and from where?
- **Writes are explicit.** Posting a comment, opening an issue or sending a message is `write` (or `irreversible`), needs the user's yes, and is denied by default (`effectsPolicy`). An agent that answers in the chat is `read` all the way.
- **APIs that are better with a key**: declare the secret, and say whether the tool also works without it (public data, lower rate limits). If you can't get a key, hand-build the fixtures in the real shape and say so.

## Contract
- **Input**: argv only (plus declared secrets as env vars). Parse flags with `node:util` `parseArgs` (strict) or Python `argparse`.
- **Output**: exactly one JSON value on stdout. Nothing else on stdout: no logs, no banners. Use stderr for debugging; it goes to the runner log and never to the model.
- **Expected failures** (not found, invalid upstream reply, rate limited): print a JSON object with an `error` field and exit 0, e.g. `{"error":"not_found","query":"..."}`. The model sees the message and can recover.
- **Exit non-zero only for bugs.** The model then sees just "tool exited with status N", which is useless to it and makes it retry blindly.
- Return only what the model needs: pick and rename fields, and cap lists (e.g. first 10). Output over 1 MiB is a failure.
- Standard library only. No `npm install` or `pip install`; package managers aren't in the runner images.
- **Shared code**: everything under `tools/` ships in the bundle, so helper modules are fine. Node: `import { x } from "./helper.ts"` (the `.ts` extension is required). Python: `import helper` works, because the script's directory is on `sys.path`.
- **Request budget**: the whole call, every request included, must finish within the tool's `timeout` (default 15s from the scaffold). Through the egress proxy, count on roughly 100–300 ms per request. Fetch in parallel (`Promise.all`, `ThreadPoolExecutor`), cap fan-out (about 10–20 requests per call), and raise `timeout` if needed.

## Environment
| Variable | Meaning |
| --- | --- |
| `<SECRET_NAME>` | each secret the tool declares |
| `HOME` | private, writable, persists for the pod's life |
| `TMPDIR` | private, writable, deleted after the call |
| `TAP_WORKSPACE` | `/workspace`, shared with the harness; for large inputs and outputs |
| `HTTPS_PROXY` | set when the tool declares egress; standard clients use it automatically |

## Node (runner-node, Node 22, TypeScript type stripping)
- Files are `.ts` run directly by `node` (type annotations only: no enums, namespaces or parameter properties).
- Use global `fetch`: it honours the proxy (`NODE_USE_ENV_PROXY=1` is set). Always pass `signal: AbortSignal.timeout(10_000)`.
- Top-level `await` works; use `import` (ESM).
- Build URLs with `URL` / `URLSearchParams` or `encodeURIComponent`, and record exactly that URL in fixtures. `URLSearchParams` encodes `,` as `%2C`, space as `+` and `'` as `%27`. To get the exact string, print it with `node -e` rather than encoding by hand.

```ts
import { parseArgs } from "node:util";
const { values } = parseArgs({ options: { city: { type: "string" } }, strict: true });
const url = new URL("https://api.example.com/v1/search");
url.searchParams.set("q", values.city!);
const res = await fetch(url, { headers: { accept: "application/json" }, signal: AbortSignal.timeout(10_000) });
if (!res.ok) {
  process.stdout.write(JSON.stringify({ error: "upstream", status: res.status }));
} else {
  const data = await res.json();
  process.stdout.write(JSON.stringify({ results: data.items.slice(0, 10).map((i: any) => ({ id: i.id, name: i.name })) }));
}
```

## Python (runner-python, Python 3.13)
- Use `urllib.request`: it honours `HTTPS_PROXY`. `requests` isn't installed.
- `argparse` with `type=float` turns `0` into `0.0`, and that is what ends up in the URL. Record fixtures accordingly, or format numbers yourself.
- `urllib.parse.urlencode` encodes `,` as `%2C` and spaces as `+`. Fixture URLs must match.
- Catch `urllib.error.HTTPError` for expected statuses and print a JSON error. Let unexpected exceptions crash (non-zero exit).

```python
import argparse, json, sys, urllib.error, urllib.parse, urllib.request
p = argparse.ArgumentParser(); p.add_argument("--city", required=True); a = p.parse_args()
url = "https://api.example.com/v1/search?" + urllib.parse.urlencode({"q": a.city})
try:
    with urllib.request.urlopen(urllib.request.Request(url, headers={"Accept": "application/json"}), timeout=10) as r:
        data = json.load(r)
    json.dump({"results": [{"id": i["id"], "name": i["name"]} for i in data["items"][:10]]}, sys.stdout)
except urllib.error.HTTPError as e:
    json.dump({"error": "upstream", "status": e.code}, sys.stdout)
```

## APIs that need headers
Some APIs reject requests without a `User-Agent` (GitHub, for example) or need `Accept`. Set them explicitly. Python's default `Python-urllib/3.x` user agent is blocked by some hosts.
