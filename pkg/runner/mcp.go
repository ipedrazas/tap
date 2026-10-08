package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ipedrazas/tap/pkg/mcp"
	"github.com/ipedrazas/tap/pkg/spec"
)

// MCPBackend is one remote MCP server session.
type MCPBackend interface {
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error)
}

// MCPDial connects to a server. header carries the declared auth.
type MCPDial func(s spec.MCPServer, header http.Header) (MCPBackend, error)

const driftInterval = 10 * time.Minute

type mcpServer struct {
	spec    spec.MCPServer
	pinned  mcp.Snapshot
	timeout time.Duration

	mu        sync.Mutex
	backend   MCPBackend
	checkedAt time.Time
	drifted   []string
}

type mcpTool struct {
	server *mcpServer
	name   string
	schema *jsonschema.Schema
}

func (r *Runner) loadMCP() error {
	r.servers = map[string]*mcpServer{}
	r.mcpTools = map[string]*mcpTool{}
	for _, s := range r.agent.MCP {
		data, err := os.ReadFile(filepath.Join(r.cfg.BundleDir, spec.MCPSnapshotPath(s.Name)))
		if err != nil {
			return fmt.Errorf("mcp %s: %w", s.Name, err)
		}
		var pinned mcp.Snapshot
		if err := json.Unmarshal(data, &pinned); err != nil {
			return fmt.Errorf("mcp %s: %w", s.Name, err)
		}
		timeout := DefaultTimeout
		if s.Timeout != "" {
			if timeout, err = time.ParseDuration(s.Timeout); err != nil {
				return err
			}
		}
		srv := &mcpServer{spec: s, pinned: pinned, timeout: timeout}
		r.servers[s.Name] = srv
		for _, t := range s.Tools {
			p, ok := pinned[t.Name]
			if !ok {
				return fmt.Errorf("mcp %s: %s is not pinned", s.Name, t.Name)
			}
			full := spec.MCPToolName(s.Name, t.Name)
			sch, err := spec.CompileSchema(full+".input.json", p.InputSchema)
			if err != nil {
				return fmt.Errorf("mcp %s: %w", full, err)
			}
			r.mcpTools[full] = &mcpTool{server: srv, name: t.Name, schema: sch}
		}
	}
	return nil
}

func (r *Runner) authHeader(s spec.MCPServer) (http.Header, error) {
	h := http.Header{}
	if s.Auth.Type == "none" {
		return h, nil
	}
	if r.cfg.Secrets == nil {
		return nil, fmt.Errorf("secret %s is not available", s.Auth.Secret)
	}
	v, err := r.cfg.Secrets(s.Auth.Secret)
	if err != nil {
		return nil, err
	}
	switch s.Auth.Type {
	case "bearer":
		h.Set("Authorization", "Bearer "+v)
	case "header":
		h.Set(s.Auth.Header, v)
	}
	return h, nil
}

// backend returns the server session, dialling on first use, and refreshes
// the drift check when it is stale.
func (r *Runner) backend(ctx context.Context, srv *mcpServer) (MCPBackend, []string, error) {
	if r.mcpOverride != nil {
		return r.mcpOverride, nil, nil
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.backend == nil {
		if r.cfg.MCP == nil {
			return nil, nil, fmt.Errorf("MCP is not configured in this runner")
		}
		h, err := r.authHeader(srv.spec)
		if err != nil {
			r.cfg.Logger.Error("mcp auth", "server", srv.spec.Name, "err", err)
			return nil, nil, fmt.Errorf("credentials for %s are not available", srv.spec.Name)
		}
		b, err := r.cfg.MCP(srv.spec, h)
		if err != nil {
			return nil, nil, err
		}
		srv.backend = b
	}
	if time.Since(srv.checkedAt) > driftInterval {
		ctx, cancel := context.WithTimeout(ctx, srv.timeout)
		defer cancel()
		live, err := srv.backend.ListTools(ctx)
		if err != nil {
			srv.backend = nil // reconnect next time
			return nil, nil, fmt.Errorf("tools/list: %w", err)
		}
		srv.drifted, srv.checkedAt = mcp.Drift(srv.pinned, live), time.Now()
		if len(srv.drifted) > 0 {
			r.cfg.Logger.Warn("mcp schema drift", "server", srv.spec.Name, "tools", srv.drifted)
		}
	}
	return srv.backend, srv.drifted, nil
}

func (r *Runner) callMCP(ctx context.Context, t *mcpTool, args map[string]any) (json.RawMessage, *CallError) {
	if err := spec.ValidateValue(t.schema, args); err != nil {
		return nil, fail(KindInvalidArgs, "%s", oneLine(err))
	}
	b, drifted, err := r.backend(ctx, t.server)
	if err != nil {
		return nil, fail(KindUpstream, "%s: %v", t.server.spec.Name, err)
	}
	if slices.Contains(drifted, t.name) {
		return nil, fail(KindDenied, "%s changed its schema on %s since review; re-run `tapctl mcp snapshot` and redeploy", t.name, t.server.spec.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, t.server.timeout)
	defer cancel()
	raw, err := b.CallTool(ctx, t.name, args)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fail(KindTimeout, "%s exceeded %s", t.name, t.server.timeout)
		}
		return nil, fail(KindUpstream, "%s", truncateErr(err))
	}
	out, isError, text, err := mcp.Shape(raw)
	switch {
	case err != nil:
		return nil, fail(KindBadOutput, "%v", err)
	case isError:
		return nil, fail(KindUpstream, "%s", text)
	case len(out) > r.cfg.OutputCap:
		return nil, fail(KindOutputTooLarge, "result exceeded %d bytes", r.cfg.OutputCap)
	}
	return out, nil
}

func truncateErr(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// stubMCP serves recorded results in fixture runs; its listing is the pinned
// snapshot, so it never drifts.
type stubMCP struct {
	pinned mcp.Snapshot
	result json.RawMessage
	calls  []string
}

func (s *stubMCP) ListTools(context.Context) ([]mcp.Tool, error) {
	var out []mcp.Tool
	for name, p := range s.pinned {
		out = append(out, mcp.Tool{Name: name, Description: p.Description, InputSchema: p.InputSchema})
	}
	return out, nil
}

func (s *stubMCP) CallTool(_ context.Context, name string, _ map[string]any) (json.RawMessage, error) {
	s.calls = append(s.calls, name)
	return s.result, nil
}
