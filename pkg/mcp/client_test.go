package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipedrazas/tap/pkg/mcp"
	"github.com/ipedrazas/tap/pkg/mcp/mcptest"
)

func fake() *mcptest.Server {
	return &mcptest.Server{
		Tools: []map[string]any{
			{"name": "a", "inputSchema": map[string]any{"type": "object"}},
			{"name": "b", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
			{"name": "c", "inputSchema": map[string]any{"type": "object"}},
		},
		Results: map[string]any{"b": map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello"}}}},
	}
}

func TestClient(t *testing.T) {
	for _, sse := range []bool{false, true} {
		srv := fake()
		srv.SSE, srv.Token = sse, "tok"
		hs := httptest.NewServer(srv)
		c := &mcp.Client{URL: hs.URL, HTTP: hs.Client(), Header: http.Header{"Authorization": {"Bearer tok"}}}
		tools, err := c.ListTools(context.Background())
		if err != nil || len(tools) != 3 {
			t.Fatalf("sse=%v list: %v %v", sse, err, tools)
		}
		raw, err := c.CallTool(context.Background(), "b", map[string]any{"q": "x"})
		if err != nil {
			t.Fatalf("sse=%v call: %v", sse, err)
		}
		out, isErr, _, err := mcp.Shape(raw)
		if err != nil || isErr || string(out) != `{"text":"hello"}` {
			t.Fatalf("sse=%v shape: %s %v %v", sse, out, isErr, err)
		}
		if _, err := c.CallTool(context.Background(), "nope", nil); err == nil {
			t.Fatal("rpc error not surfaced")
		}
		hs.Close()
	}
}

func TestReinitialisesExpiredSession(t *testing.T) {
	srv := fake()
	srv.ExpireOnce = true
	hs := httptest.NewServer(srv)
	defer hs.Close()
	c := &mcp.Client{URL: hs.URL, HTTP: hs.Client()}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.Inits != 2 {
		t.Fatalf("want re-initialize, got %d inits", srv.Inits)
	}
}

func TestPinAndDrift(t *testing.T) {
	live := []mcp.Tool{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)},
		{Name: "b", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	if _, err := mcp.Pin(live, []string{"a", "missing"}); err == nil {
		t.Fatal("pinning an absent tool must fail")
	}
	snap, err := mcp.Pin(live, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	// Key order and whitespace are not drift.
	same := []mcp.Tool{{Name: "a", InputSchema: json.RawMessage(`{ "properties": {"x": {"type": "string"}}, "type": "object" }`)}}
	if d := mcp.Drift(snap, same); len(d) != 0 {
		t.Fatalf("false drift: %v", d)
	}
	changed := []mcp.Tool{{Name: "a", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`)}}
	if d := mcp.Drift(snap, changed); len(d) != 1 {
		t.Fatalf("missed drift: %v", d)
	}
	if d := mcp.Drift(snap, nil); len(d) != 1 {
		t.Fatalf("removed tool is drift: %v", d)
	}
}

func TestShape(t *testing.T) {
	for raw, want := range map[string]string{
		`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`: `{"text":"a\nb"}`,
		`{"content":[],"structuredContent":{"n":1}}`:                          `{"n":1}`,
		`{"content":[{"type":"image","data":"x","mimeType":"image/png"}]}`:    `{"content":[{"data":"x","mimeType":"image/png","type":"image"}]}`,
	} {
		out, _, _, err := mcp.Shape(json.RawMessage(raw))
		if err != nil || string(out) != want {
			t.Errorf("%s: got %s %v", raw, out, err)
		}
	}
	if _, isErr, text, _ := mcp.Shape(json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"boom"}]}`)); !isErr || text != "boom" {
		t.Fatal("isError not detected")
	}
}
