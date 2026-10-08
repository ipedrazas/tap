package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const runnerRef = "registry.hiddenfield.dev/tap/runner-node@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func testPlatform() *Platform {
	return &Platform{
		Model:   PlatformModel{Routes: []string{"agent", "sim"}},
		Runners: []RunnerImage{{Name: "runner-node", Image: runnerRef, Interpreters: []string{"node"}}},
	}
}

const baseAgent = `apiVersion: tavon.ai/agent/v1
kind: Agent
metadata: { name: test-agent, version: 1.0.0, owner: me, description: test }
harness:
  api: v1
  model: { provider: gateway, name: agent }
effectsPolicy: { write: deny, irreversible: deny }
prompt: system.md
skills: [skills/demo]
runner:
  image: ` + runnerRef + `
tools:
  - name: fetch_thing
    description: Fetch a thing
    input_schema:
      type: object
      properties:
        id: { type: string, pattern: "^T-[0-9]+$" }
      required: [id]
      additionalProperties: false
    exec: ["node", "tools/fetch.ts", "--id", "{{id}}"]
    secrets: [API_TOKEN]
    egress: [api.example.com:443]
    effects: read
secrets:
  API_TOKEN: { from: vault://demo/token }
`

const baseFixture = `tool: fetch_thing
cases:
  - name: ok
    args: { id: T-1 }
    expect: { ok: true }
`

// writeBundle materialises a bundle in a temp dir. edit rewrites agent.yaml.
func writeBundle(t *testing.T, agent string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"agent.yaml":                  agent,
		"system.md":                   "prompt",
		"skills/demo/SKILL.md":        "---\nname: demo\ndescription: demo skill\n---\nbody\n",
		"tools/fetch.ts":              "console.log('{}')",
		"tests/fetch_thing.test.yaml": baseFixture,
	}
	for k, v := range extra {
		if v == "" {
			delete(files, k)
		} else {
			files[k] = v
		}
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, dir string) *Bundle {
	t.Helper()
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

func TestValidBundle(t *testing.T) {
	b := load(t, writeBundle(t, baseAgent, nil))
	if f := Validate(b, testPlatform()); len(f) != 0 {
		t.Fatalf("expected no findings, got %v", f)
	}
}

func TestEchoAgentReference(t *testing.T) {
	b := load(t, "../../agents/echo-agent")
	p := testPlatform()
	p.Runners[0].Image = b.Agent.Runner.Image
	if f := Validate(b, p); len(f) != 0 {
		t.Fatalf("echo-agent: %v", f)
	}
}

func TestRules(t *testing.T) {
	tests := []struct {
		name  string
		agent func(string) string
		extra map[string]string
		rule  int
		want  string
	}{
		{
			name:  "shell interpreter",
			agent: replace(`["node", "tools/fetch.ts"`, `["sh", "tools/fetch.ts"`),
			rule:  1, want: "is a shell",
		},
		{
			name:  "inline code flag",
			agent: replace(`["node", "tools/fetch.ts", "--id", "{{id}}"]`, `["node", "-e", "x", "{{id}}"]`),
			rule:  1, want: "evaluates inline code",
		},
		{
			name:  "interpreter missing from runner",
			agent: replace(`["node", "tools/fetch.ts"`, `["python3", "tools/fetch.ts"`),
			rule:  1, want: "not available in runner-node",
		},
		{
			name:  "script outside tools",
			agent: replace(`tools/fetch.ts`, `../fetch.ts`),
			rule:  1, want: "must be a script under tools/",
		},
		{
			name:  "missing script",
			agent: replace(`tools/fetch.ts`, `tools/nope.ts`),
			rule:  1, want: "does not exist",
		},
		{
			name:  "partial template",
			agent: replace(`"{{id}}"`, `"id={{id}}"`),
			rule:  2, want: "whole argv element",
		},
		{
			name:  "unknown template",
			agent: replace(`"{{id}}"`, `"{{other}}"`),
			rule:  2, want: "not a property",
		},
		{
			name:  "unconstrained string",
			agent: replace(`id: { type: string, pattern: "^T-[0-9]+$" }`, `id: { type: string }`),
			extra: map[string]string{"tests/fetch_thing.test.yaml": baseFixture},
			rule:  2, want: "without pattern",
		},
		{
			name:  "optional templated arg",
			agent: replace(`required: [id]`, `required: []`),
			rule:  2, want: "must be required or have a default",
		},
		{
			name: "unused property",
			agent: replace(`id: { type: string, pattern: "^T-[0-9]+$" }`,
				"id: { type: string, pattern: \"^T-[0-9]+$\" }\n        extra: { type: integer }"),
			rule: 2, want: "never substituted",
		},
		{
			name:  "undeclared secret",
			agent: replace(`secrets: [API_TOKEN]`, `secrets: [API_TOKEN, OTHER]`),
			rule:  3, want: `"OTHER" is not declared`,
		},
		{
			name:  "unused secret",
			agent: replace(`secrets: [API_TOKEN]`, `secrets: []`),
			rule:  3, want: "declared but not used",
		},
		{
			name:  "ip egress",
			agent: replace(`api.example.com:443`, `10.0.0.1:443`),
			rule:  4, want: "not an IP",
		},
		{
			name:  "missing fixture",
			extra: map[string]string{"tests/fetch_thing.test.yaml": ""},
			rule:  5, want: "no fixture",
		},
		{
			name:  "fixture args disagree with schema",
			extra: map[string]string{"tests/fetch_thing.test.yaml": strings.Replace(baseFixture, "T-1", "nope", 1)},
			rule:  5, want: "do not match input_schema",
		},
		{
			name:  "uncurated runner",
			agent: replace(runnerRef, strings.Replace(runnerRef, "1111", "2222", 1)),
			rule:  6, want: "not a curated runner",
		},
		{
			name:  "double underscore tool name",
			agent: replace(`name: fetch_thing`, `name: fetch__thing`),
			extra: map[string]string{"tests/fetch_thing.test.yaml": strings.Replace(baseFixture, "fetch_thing", "fetch__thing", 1)},
			rule:  8, want: "reserved for MCP",
		},
		{
			name:  "missing prompt",
			extra: map[string]string{"system.md": ""},
			rule:  0, want: "system.md does not exist",
		},
		{
			name:  "unknown model route",
			agent: replace(`name: agent }`, `name: gpt }`),
			rule:  0, want: "not a model gateway route",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := baseAgent
			if tc.agent != nil {
				agent = tc.agent(agent)
			}
			b, err := Load(writeBundle(t, agent, tc.extra))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("expected schema error")
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			findings := Validate(b, testPlatform())
			for _, f := range findings {
				if f.Rule == tc.rule && strings.Contains(f.Msg, tc.want) {
					return
				}
			}
			t.Fatalf("want rule %d %q, got %v", tc.rule, tc.want, findings)
		})
	}
}

func TestSchemaRejects(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"wildcard egress":           replace(`api.example.com:443`, `*.example.com:443`),
		"additionalProperties true": replace(`additionalProperties: false`, `additionalProperties: true`),
		"unknown field":             replace(`prompt: system.md`, "prompt: system.md\nextra: 1"),
		"runner by tag":             replace(runnerRef, "registry.hiddenfield.dev/tap/runner-node:latest"),
		"bad secret source":         replace(`vault://demo/token`, `file:///etc/passwd`),
		"bad effects":               replace(`effects: read`, `effects: maybe`),
		"bearer without secret": func(s string) string {
			return s + "mcp:\n  - name: gh\n    url: https://mcp.example.com/\n    transport: streamable-http\n    auth: { type: bearer }\n    tools: [{ name: x, effects: read }]\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeBundle(t, edit(baseAgent), nil)); err == nil {
				t.Fatal("expected schema error")
			}
		})
	}
}

const mcpBlock = `mcp:
  - name: github
    url: https://api.githubcopilot.com/mcp/
    transport: streamable-http
    auth: { type: bearer, secret: GITHUB_TOKEN }
    tools:
      - { name: get_issue, effects: read }
`

func mcpAgent() string {
	return strings.Replace(baseAgent, "secrets:\n  API_TOKEN", mcpBlock+"secrets:\n  GITHUB_TOKEN: { from: vault://demo/gh }\n  API_TOKEN", 1)
}

func TestMCPRules(t *testing.T) {
	snapshot := `{"get_issue": {"type": "object", "properties": {"number": {"type": "integer"}}, "required": ["number"]}}`
	fixture := "tool: github__get_issue\ncases:\n  - name: ok\n    args: { number: 1 }\n    mcp_response: { content: [] }\n    expect: { ok: true }\n"

	t.Run("valid", func(t *testing.T) {
		b := load(t, writeBundle(t, mcpAgent(), map[string]string{
			"mcp/github.tools.json":                snapshot,
			"tests/mcp/github/get_issue.test.yaml": fixture,
		}))
		if f := Validate(b, testPlatform()); len(f) != 0 {
			t.Fatalf("expected no findings, got %v", f)
		}
	})
	cases := map[string]struct {
		extra map[string]string
		rule  int
		want  string
	}{
		"no snapshot": {
			extra: map[string]string{"tests/mcp/github/get_issue.test.yaml": fixture},
			rule:  10, want: "is missing",
		},
		"snapshot pins extra tool": {
			extra: map[string]string{
				"mcp/github.tools.json":                `{"get_issue": {}, "delete_repo": {}}`,
				"tests/mcp/github/get_issue.test.yaml": fixture,
			},
			rule: 10, want: "not in the allowlist",
		},
		"no fixture": {
			extra: map[string]string{"mcp/github.tools.json": snapshot},
			rule:  12, want: "no fixture",
		},
		"fixture without recorded response": {
			extra: map[string]string{
				"mcp/github.tools.json":                snapshot,
				"tests/mcp/github/get_issue.test.yaml": strings.Replace(fixture, "    mcp_response: { content: [] }\n", "", 1),
			},
			rule: 12, want: "recorded mcp_response",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := load(t, writeBundle(t, mcpAgent(), tc.extra))
			findings := Validate(b, testPlatform())
			for _, f := range findings {
				if f.Rule == tc.rule && strings.Contains(f.Msg, tc.want) {
					return
				}
			}
			t.Fatalf("want rule %d %q, got %v", tc.rule, tc.want, findings)
		})
	}
	t.Run("undeclared auth secret", func(t *testing.T) {
		agent := strings.Replace(mcpAgent(), "  GITHUB_TOKEN: { from: vault://demo/gh }\n", "", 1)
		b := load(t, writeBundle(t, agent, map[string]string{"mcp/github.tools.json": snapshot, "tests/mcp/github/get_issue.test.yaml": fixture}))
		for _, f := range Validate(b, testPlatform()) {
			if f.Rule == 9 {
				return
			}
		}
		t.Fatal("expected rule 9")
	})
}

func TestDiff(t *testing.T) {
	base := load(t, writeBundle(t, baseAgent, nil))

	t.Run("unchanged", func(t *testing.T) {
		head := load(t, writeBundle(t, baseAgent, nil))
		d := Diff(base, head)
		if len(d.Changes) != 0 || d.VersionUnchanged {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("new agent widens", func(t *testing.T) {
		if d := Diff(nil, base); !d.Widens() {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("narrowing only", func(t *testing.T) {
		agent := strings.NewReplacer(
			"version: 1.0.0", "version: 1.0.1",
			"    egress: [api.example.com:443]\n", "",
		).Replace(baseAgent)
		d := Diff(base, load(t, writeBundle(t, agent, nil)))
		if d.Widens() || len(d.Changes) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
	for name, edit := range map[string]func(string) string{
		"effects":    replace("effects: read", "effects: irreversible"),
		"egress":     replace("egress: [api.example.com:443]", "egress: [api.example.com:443, other.example.com:443]"),
		"policy":     replace("write: deny", "write: auto"),
		"mcp server": func(s string) string { return mcpAgent() },
	} {
		t.Run("widens "+name, func(t *testing.T) {
			agent := strings.Replace(edit(baseAgent), "version: 1.0.0", "version: 1.1.0", 1)
			head, err := Load(writeBundle(t, agent, nil))
			if err != nil {
				t.Fatal(err)
			}
			d := Diff(base, head)
			if !d.Widens() || d.VersionUnchanged {
				t.Fatalf("got %+v", d)
			}
		})
	}
	t.Run("version not bumped", func(t *testing.T) {
		head := load(t, writeBundle(t, replace("effects: read", "effects: write")(baseAgent), nil))
		if d := Diff(base, head); !d.VersionUnchanged {
			t.Fatalf("got %+v", d)
		}
	})
}

func replace(old, new string) func(string) string {
	return func(s string) string {
		if !strings.Contains(s, old) {
			panic("test edit does not apply: " + old)
		}
		return strings.Replace(s, old, new, 1)
	}
}
