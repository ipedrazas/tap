package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

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
// secrets and nothing else.
func RunFixtures(ctx context.Context, r *Runner, fixtures map[string][]spec.FixtureFile) []CaseResult {
	var results []CaseResult
	for _, tool := range slices.Sorted(maps.Keys(fixtures)) {
		for _, f := range fixtures[tool] {
			for i, c := range f.Fixture.Cases {
				res := CaseResult{File: f.Path, Tool: tool, Case: c.Name}
				switch {
				case len(c.HTTP) > 0:
					res.Skipped = "needs recorded HTTP responses (mock egress proxy, phase 3)"
				case c.MCPResponse != nil:
					res.Skipped = "MCP fixtures run in phase 5"
				default:
					stub := c.Secrets
					rr := *r
					rr.cfg.Secrets = func(name string) (string, error) {
						if v, ok := stub[name]; ok {
							return v, nil
						}
						return "", fmt.Errorf("fixture provides no stub for %s", name)
					}
					args := map[string]any{}
					maps.Copy(args, c.Args)
					res.Resp = rr.Call(ctx, Request{Tool: tool, Args: args, CallID: fmt.Sprintf("fixture-%s-%d", tool, i)})
					res.Failure = check(c.Expect, res.Resp)
				}
				results = append(results, res)
			}
		}
	}
	return results
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
