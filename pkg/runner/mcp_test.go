package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ipedrazas/tap/pkg/mcp"
	"github.com/ipedrazas/tap/pkg/mcp/mcptest"
	"github.com/ipedrazas/tap/pkg/spec"
)

const mcpAgent = `apiVersion: tavon.ai/agent/v1
kind: Agent
metadata: { name: unit, version: 0.0.1, owner: me, description: unit }
harness: { api: v1, model: { provider: gateway, name: sim } }
effectsPolicy: { write: deny, irreversible: deny }
prompt: system.md
runner:
  image: registry.hiddenfield.dev/tap/runner-node@sha256:1111111111111111111111111111111111111111111111111111111111111111
mcp:
  - name: wiki
    url: https://mcp.example.com/mcp
    transport: streamable-http
    auth: { type: bearer, secret: WIKI_TOKEN }
    tools:
      - { name: ask, effects: read }
      - { name: fail, effects: read }
secrets:
  WIKI_TOKEN: { from: vault://unit/wiki }
`

const pinned = `{
  "ask": {"description": "Ask", "inputSchema": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}},
  "fail": {"description": "Fails", "inputSchema": {"type": "object"}}
}`

func mcpRunner(t *testing.T, srv *mcptest.Server) *Runner {
	t.Helper()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(mcpAgent), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "mcp"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "mcp", "wiki.tools.json"), []byte(pinned), 0o644))
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	r, err := New(Config{
		BundleDir: dir, Workspace: t.TempDir(), TmpDir: t.TempDir(),
		Secrets: func(name string) (string, error) { return "tok-" + name, nil },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCP: func(s spec.MCPServer, h http.Header) (MCPBackend, error) {
			return &mcp.Client{URL: hs.URL, Header: h, HTTP: hs.Client()}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func liveTools(askSchema string) []map[string]any {
	var s map[string]any
	_ = json.Unmarshal([]byte(askSchema), &s)
	return []map[string]any{
		{"name": "ask", "inputSchema": s},
		{"name": "fail", "inputSchema": map[string]any{"type": "object"}},
		{"name": "delete_everything", "inputSchema": map[string]any{"type": "object"}},
	}
}

const askSchema = `{"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}`

func fakeWiki() *mcptest.Server {
	return &mcptest.Server{
		Token: "tok-WIKI_TOKEN",
		SSE:   true,
		Tools: liveTools(askSchema),
		Results: map[string]any{
			"ask":               map[string]any{"content": []any{map[string]any{"type": "text", "text": "42"}}},
			"fail":              map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "repo not indexed"}}},
			"delete_everything": map[string]any{"content": []any{}},
		},
	}
}

func TestMCPCall(t *testing.T) {
	srv := fakeWiki()
	r := mcpRunner(t, srv)
	resp, out := call(t, r, "wiki__ask", map[string]any{"q": "meaning of life"})
	if !resp.OK || out["text"] != "42" {
		t.Fatalf("got %+v %v", resp, out)
	}
	if len(srv.Calls) != 1 || srv.Calls[0]["name"] != "ask" {
		t.Fatalf("server saw %v", srv.Calls)
	}
}

func TestMCPAllowlistAndValidation(t *testing.T) {
	srv := fakeWiki()
	r := mcpRunner(t, srv)
	if resp, _ := call(t, r, "wiki__delete_everything", nil); resp.OK || resp.Error.Kind != KindDenied {
		t.Fatalf("unlisted MCP tool must be denied: %+v", resp)
	}
	if resp, _ := call(t, r, "wiki__ask", map[string]any{}); resp.OK || resp.Error.Kind != KindInvalidArgs {
		t.Fatalf("args must be validated against the pinned schema: %+v", resp)
	}
	if resp, _ := call(t, r, "wiki__fail", nil); resp.OK || resp.Error.Kind != KindUpstream || resp.Error.Message != "repo not indexed" {
		t.Fatalf("isError: %+v %+v", resp, resp.Error)
	}
	if len(srv.Calls) != 1 {
		t.Fatalf("only the isError call should reach the server, saw %v", srv.Calls)
	}
}

func TestMCPDriftBlocksTool(t *testing.T) {
	srv := fakeWiki()
	srv.Tools = liveTools(`{"type": "object", "properties": {"q": {"type": "string"}, "delete": {"type": "boolean"}}, "required": ["q"]}`)
	r := mcpRunner(t, srv)
	resp, _ := call(t, r, "wiki__ask", map[string]any{"q": "x"})
	if resp.OK || resp.Error.Kind != KindDenied {
		t.Fatalf("drifted tool must be denied: %+v", resp)
	}
	if resp, _ := call(t, r, "wiki__fail", nil); resp.Error == nil || resp.Error.Kind != KindUpstream {
		t.Fatalf("undrifted tool should still be callable: %+v", resp)
	}
}

func TestMCPAuthFailure(t *testing.T) {
	srv := fakeWiki()
	srv.Token = "something-else"
	r := mcpRunner(t, srv)
	if resp, _ := call(t, r, "wiki__ask", map[string]any{"q": "x"}); resp.OK || resp.Error.Kind != KindUpstream {
		t.Fatalf("got %+v", resp)
	}
}

func TestMCPFixtures(t *testing.T) {
	r := mcpRunner(t, fakeWiki())
	fixtures := map[string][]spec.FixtureFile{"wiki__ask": {{Path: "tests/mcp/wiki/ask.test.yaml", Fixture: spec.Fixture{Tool: "wiki__ask", Cases: []spec.FixtureCase{
		{Name: "ok", Args: map[string]any{"q": "x"}, MCPResponse: json.RawMessage(`{"content":[{"type":"text","text":"recorded"}]}`),
			Expect: spec.Expect{OK: true, OutputSchema: json.RawMessage(`{"properties":{"text":{"const":"recorded"}}}`)}},
		{Name: "bad args", Args: map[string]any{}, MCPResponse: json.RawMessage(`{"content":[]}`),
			Expect: spec.Expect{OK: false, ErrorKind: KindInvalidArgs}},
	}}}}}
	for _, res := range RunFixtures(context.Background(), r, fixtures, nil) {
		if !res.Passed() {
			t.Errorf("%s: %s %s", res.Case, res.Failure, res.Skipped)
		}
	}
}
