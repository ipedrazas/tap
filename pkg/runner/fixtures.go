package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/spec"
)

type CaseResult struct {
	File, Tool, Case string
	// Skipped explains why a case was not run (e.g. it needs the mock egress proxy).
	Skipped string
	Failure string
	Resp    Response
}

func (c CaseResult) Passed() bool { return c.Skipped == "" && c.Failure == "" }

// RunFixtures runs every case against the real tools, with each case's stub
// secrets and nothing else. Tools that declare egress go through mock, which
// serves the case's recorded HTTP responses and enforces the allowlist.
func RunFixtures(ctx context.Context, r *Runner, fixtures map[string][]spec.FixtureFile, mock *egress.Mock) []CaseResult {
	var results []CaseResult
	for _, tool := range slices.Sorted(maps.Keys(fixtures)) {
		for _, f := range fixtures[tool] {
			for i, c := range f.Fixture.Cases {
				res := CaseResult{File: f.Path, Tool: tool, Case: c.Name}
				t, isScript := r.tools[tool]
				switch {
				case len(c.HTTP) > 0 && (!isScript || len(t.spec.Egress) == 0):
					res.Failure = "case records HTTP responses but the tool declares no egress"
				case len(c.HTTP) > 0 && mock == nil:
					res.Skipped = "needs the mock egress proxy"
				case c.MCPResponse != nil:
					mt, ok := r.mcpTools[tool]
					if !ok {
						res.Failure = "mcp_response on a tool that is not an MCP tool"
						break
					}
					rr := *r
					stub := &stubMCP{pinned: mt.server.pinned, result: c.MCPResponse}
					rr.mcpOverride = stub
					rr.cfg.Secrets = func(name string) (string, error) { return c.Secrets[name], nil }
					args := map[string]any{}
					maps.Copy(args, c.Args)
					res.Resp = rr.Call(ctx, Request{Tool: tool, Args: args, CallID: fmt.Sprintf("fixture-%s-%d", tool, i)})
					res.Failure = check(c.Expect, res.Resp)
					if res.Failure == "" && res.Resp.OK && len(stub.calls) != 1 {
						res.Failure = fmt.Sprintf("expected one tools/call, got %d", len(stub.calls))
					}
				default:
					stub := c.Secrets
					rr := *r
					rr.cfg.Secrets = func(name string) (string, error) {
						if v, ok := stub[name]; ok {
							return v, nil
						}
						return "", fmt.Errorf("fixture provides no stub for %s", name)
					}
					rr.cfg.Egress = nil
					if mock != nil && isScript {
						mock.Prepare(t.spec.Egress, c.HTTP)
						rr.cfg.Egress = func(string, time.Duration) ([]string, error) { return mock.Env(), nil }
					}
					args := map[string]any{}
					maps.Copy(args, c.Args)
					res.Resp = rr.Call(ctx, Request{Tool: tool, Args: args, CallID: fmt.Sprintf("fixture-%s-%d", tool, i)})
					res.Failure = check(c.Expect, res.Resp)
					if mock != nil {
						if p := mock.Problems(); len(p) > 0 && res.Failure == "" {
							res.Failure = "egress: " + strings.Join(p, "; ")
						}
					}
				}
				results = append(results, res)
			}
		}
	}
	return results
}

// RunCheckFixtures runs every case of every deterministic check, with each
// case's stub secrets and recorded HTTP, and compares pass and reason.
// Results are reported under the name check:<id>.
func RunCheckFixtures(ctx context.Context, r *Runner, fixtures map[string][]spec.CheckFixtureFile, mock *egress.Mock) []CaseResult {
	var results []CaseResult
	for _, id := range slices.Sorted(maps.Keys(fixtures)) {
		for _, f := range fixtures[id] {
			for i, c := range f.Fixture.Cases {
				res := CaseResult{File: f.Path, Tool: spec.CheckScope(id), Case: c.Name}
				t, ok := r.checks[id]
				switch {
				case !ok:
					res.Failure = "no deterministic check " + id + " in acceptance.yaml"
				case len(c.HTTP) > 0 && len(t.spec.Egress) == 0:
					res.Failure = "case records HTTP responses but the check declares no egress"
				case len(c.HTTP) > 0 && mock == nil:
					res.Skipped = "needs the mock egress proxy"
				default:
					rr := *r
					stub := c.Secrets
					rr.cfg.Secrets = func(name string) (string, error) {
						if v, ok := stub[name]; ok {
							return v, nil
						}
						return "", fmt.Errorf("fixture provides no stub for %s", name)
					}
					rr.cfg.Egress = nil
					if mock != nil {
						mock.Prepare(t.spec.Egress, c.HTTP)
						rr.cfg.Egress = func(string, time.Duration) ([]string, error) { return mock.Env(), nil }
					}
					cr := rr.Check(ctx, CheckRequest{Check: id, Task: c.Task, Output: c.Output, Attempt: 1, CallID: fmt.Sprintf("fixture-check-%s-%d", id, i)})
					res.Resp = Response{CallID: cr.CallID, OK: cr.OK, Error: cr.Error, DurationMS: cr.DurationMS}
					res.Failure = expectCheck(c.Expect, cr)
					if mock != nil {
						if p := mock.Problems(); len(p) > 0 && res.Failure == "" {
							res.Failure = "egress: " + strings.Join(p, "; ")
						}
					}
				}
				results = append(results, res)
			}
		}
	}
	return results
}

func expectCheck(want spec.CheckExpect, got CheckResponse) string {
	if !got.OK {
		return fmt.Sprintf("check errored: %s: %s", got.Error.Kind, got.Error.Message)
	}
	if got.Result.Pass != want.Pass {
		return fmt.Sprintf("want pass=%v, got pass=%v: %s", want.Pass, got.Result.Pass, got.Result.Reason)
	}
	if want.Reason != "" {
		re, err := regexp.Compile(want.Reason)
		if err != nil {
			return fmt.Sprintf("expect.reason: %v", err)
		}
		if !re.MatchString(got.Result.Reason) {
			return fmt.Sprintf("reason %q does not match %q", got.Result.Reason, want.Reason)
		}
	}
	return ""
}

func check(want spec.Expect, got Response) string {
	if got.OK != want.OK {
		if got.Error != nil {
			return fmt.Sprintf("want ok=%v, got %s: %s", want.OK, got.Error.Kind, got.Error.Message)
		}
		return fmt.Sprintf("want ok=%v, got ok=%v", want.OK, got.OK)
	}
	if want.ErrorKind != "" && (got.Error == nil || got.Error.Kind != want.ErrorKind) {
		return fmt.Sprintf("want error_kind %s, got %+v", want.ErrorKind, got.Error)
	}
	if want.OutputSchema != nil {
		s, err := spec.CompileSchema("output.json", want.OutputSchema)
		if err != nil {
			return fmt.Sprintf("output_schema: %v", err)
		}
		var v any
		if err := json.Unmarshal(got.Output, &v); err != nil {
			return fmt.Sprintf("output: %v", err)
		}
		if err := spec.ValidateValue(s, v); err != nil {
			return fmt.Sprintf("output %s does not match output_schema: %s", got.Output, oneLine(err))
		}
	}
	return ""
}
