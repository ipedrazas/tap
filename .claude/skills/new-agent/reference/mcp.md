# Remote MCP servers

An agent can use tools from a remote MCP server (Streamable HTTP) instead of, or alongside, script tools. The runner is the MCP client: it connects through the egress proxy and holds the auth secret. The harness only sees the pinned tool definitions.

## Declaring a server

```yaml
mcp:
  - name: deepwiki                      # ^[a-z][a-z0-9_]{2,31}$
    url: https://mcp.deepwiki.com/mcp   # https only; its host:port is the server's egress
    transport: streamable-http
    auth: { type: none }                # or { type: bearer, secret: X } or { type: header, header: X-Api-Key, secret: X }
    timeout: 60s                        # per call; MCP tools that call an LLM can be slow
    tools:                              # explicit allowlist; everything else the server offers is invisible
      - { name: read_wiki_structure, effects: read }
      - { name: ask_wiki_question, effects: read }
```

Start with `task agent:mcp-list AGENT=<name>`. It lists every tool the server offers, with the server's read-only and destructive hints, and marks what you've allowlisted. Report the tools you left out and why.

Prefer `tapctl mcp list` and `tapctl mcp call` to raw HTTP. If you do probe a server with `curl`, Streamable HTTP needs `Accept: application/json, text/event-stream` and a JSON-RPC `initialize` first, and the reply may be an SSE stream even for a single response.

Rules:
- The model sees each tool as `<server>__<tool>`, so the full name must fit in 64 characters. MCP tool names may only use `[A-Za-z0-9_-]`.
- Allowlist only what the agent needs. Servers often offer write or admin tools next to the read ones.
- **Output size**: the 1 MiB output cap applies to MCP results too, and you can't trim an MCP tool's output. Leave out tools that return whole documents (DeepWiki's `read_wiki_contents` returns 140–635 KB), or wrap the need in a script tool that fetches and trims.
- Choose `effects` from what the tool does, not from its name. `tapctl mcp snapshot` warns when the server's annotations say a tool you declared `read` isn't read-only.
- `auth.secret` must be declared in top-level `secrets`, like any secret. Locally it comes from `.env.<agent>`.
- No `egress` field is needed: it's derived from `url`. If the server redirects to another host, use the final URL.

## Pinning schemas

The reviewed bundle must not change behaviour when the server changes. After declaring or changing a server's allowlist:

```bash
task agent:mcp-snapshot AGENT=<name>
```

This connects to the live server, keeps only the allowlisted tools, and writes `mcp/<server>.tools.json` (`{tool: {description, inputSchema}}`). Read it:
- **description:** the model sees it verbatim, so check it doesn't instruct the model to do anything odd. It came from a third party.
- **inputSchema:** the runner validates every call against it.

At runtime the runner compares the live schemas with the pinned ones every 10 minutes and refuses any tool whose schema changed (`denied: ... changed its schema since review`). To accept a change, re-snapshot, review the diff and bump `metadata.version`. `task agent:mcp-check AGENT=<name>` reports drift without writing.

## Fixtures

One file per allowlisted tool at `tests/mcp/<server>/<tool>.test.yaml`. `tool:` is the full name. Every case needs `mcp_response`, a recorded `CallToolResult`. Nothing is called live during tests.

Record real results with:

```bash
task agent:mcp-call AGENT=<name> TOOL=<server>__<tool> ARGS='{"repoName":"facebook/react"}'
```

Paste the printed JSON (trimmed if long) as `mcp_response`:

```yaml
tool: deepwiki__read_wiki_structure
cases:
  - name: lists topics
    args: { repoName: facebook/react }
    mcp_response:                       # recorded with task agent:mcp-call, then trimmed
      content:
        - { type: text, text: "1. Overview\n2. Reconciler\n..." }
      structuredContent: { result: "1. Overview\n2. Reconciler\n..." }
    expect:
      ok: true
      output_schema: { type: object, required: [result] }   # structuredContent wins: the model gets {"result": ...}
  - name: missing required argument
    args: {}
    mcp_response: { content: [] }
    expect: { ok: false, error_kind: invalid_args }
```

What the model receives:
- text-only content → `{"text": "<joined text>"}`;
- `structuredContent` → that object;
- anything else → `{"content": [...]}`;
- `isError: true` → a failed call with `error_kind: upstream` and the error text.

Check the recorded result for `structuredContent` before writing `output_schema`: many servers (DeepWiki included) send both, and then the model gets the structured object, not `{"text": ...}`. Test an `isError` result with `expect: { ok: false, error_kind: upstream }`.

## MCP-only agents

`tools` is optional. If the agent has no script tools, delete the scaffold's example tool entry, `tools/` and `tests/example_lookup.test.yaml`.
