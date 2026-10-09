---
name: repo-eval-agent
runner: runner-node
reference: repo-agent
egress: mcp.deepwiki.com:443
secrets:
effects: read
---
An agent that explains open-source GitHub repositories: what a repository is for, how its code is organised and how a given part works. Use the DeepWiki remote MCP server (https://mcp.deepwiki.com/mcp, no authentication) instead of writing tools. Allowlist only the tools needed to answer questions about a repository; avoid tools whose responses are very large.
