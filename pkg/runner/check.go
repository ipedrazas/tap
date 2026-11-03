package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/ipedrazas/tap/pkg/spec"
)

// Deterministic checks (acceptance.yaml) run exactly like tools: their own
// uid, a clean environment with only their secrets, a per-call egress
// credential for their own scope (check:<id>), the timeout and output cap.
// The differences: the harness calls them on POST /v1/check, never the
// model; and they take no argv slots but read one JSON document on stdin,
//
//	{"task": "...", "output": "...", "attempt": 1}
//
// and print one on stdout:
//
//	{"pass": true|false, "reason": "...", "details": ..., "evidence": ...}
//
// The task and output are the producing agent's input and answer: untrusted.
// A check that exits non-zero or prints anything else is an error, which the
// verifier counts as a failure.

// CheckRequest asks for one deterministic check of one answer.
type CheckRequest struct {
	Check   string `json:"check"`
	Task    string `json:"task"`
	Output  string `json:"output"`
	Attempt int    `json:"attempt"`
	CallID  string `json:"call_id"`
}

// CheckResult is what a check prints.
type CheckResult struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason"`
	// Details is free-form, structured detail (e.g. the failing URLs).
	Details json.RawMessage `json:"details,omitempty"`
	// Evidence is passed to judged criteria that use this check.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

type CheckResponse struct {
	CallID     string       `json:"call_id"`
	OK         bool         `json:"ok"`
	Result     *CheckResult `json:"result,omitempty"`
	Error      *CallError   `json:"error,omitempty"`
	DurationMS int64        `json:"duration_ms"`
}

// MaxCheckInput bounds a check request (the task plus the answer).
const MaxCheckInput = 4 << 20

// loadChecks prepares the acceptance spec's deterministic checks. Check i
// gets uid ToolUIDBase+len(tools)+i: no tool shares it.
func (r *Runner) loadChecks(defTimeout time.Duration) error {
	for i, c := range r.agent.Acceptance.Deterministic() {
		if len(r.cfg.Interpreters) > 0 && !slices.Contains(r.cfg.Interpreters, c.Exec[0]) {
			return fmt.Errorf("check %s: interpreter %q is not in this runner image", c.ID, c.Exec[0])
		}
		for _, name := range c.Secrets {
			if spec.ReservedEnv(name) {
				return fmt.Errorf("check %s: secret name %s is reserved", c.ID, name)
			}
		}
		timeout := defTimeout
		if c.Timeout != "" {
			var err error
			if timeout, err = time.ParseDuration(c.Timeout); err != nil {
				return fmt.Errorf("check %s: %w", c.ID, err)
			}
		}
		scope := spec.CheckScope(c.ID)
		r.checks[c.ID] = &tool{
			index: len(r.agent.Tools) + i,
			scope: scope,
			home:  "check-" + c.ID,
			spec:  spec.Tool{Name: scope, Exec: c.Exec, Secrets: c.Secrets, Egress: c.Egress, Effects: spec.EffectRead},
			// The runner's per-call budget: a check usually fetches many pages.
			timeout: timeout,
		}
		r.longest = max(r.longest, timeout)
	}
	return nil
}

// Checks lists the deterministic check ids this runner can run.
func (r *Runner) Checks() []string {
	var ids []string
	for id := range r.checks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Check runs one deterministic check end to end and always returns a response.
func (r *Runner) Check(ctx context.Context, req CheckRequest) CheckResponse {
	start := time.Now()
	c := r.calls
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.started[id] = start
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.started, id)
		c.mu.Unlock()
	}()
	res, cerr, exitCode := r.check(ctx, req)
	resp := CheckResponse{CallID: req.CallID, DurationMS: time.Since(start).Milliseconds()}
	if cerr != nil {
		resp.Error = cerr
	} else {
		resp.OK, resp.Result = true, res
	}
	r.auditCheck(req, resp, exitCode)
	return resp
}

func (r *Runner) check(ctx context.Context, req CheckRequest) (*CheckResult, *CallError, int) {
	t, ok := r.checks[req.Check]
	if !ok {
		return nil, fail(KindDenied, "unknown check %q", req.Check), -1
	}
	input, err := json.Marshal(map[string]any{"task": req.Task, "output": req.Output, "attempt": req.Attempt})
	if err != nil || len(input) > MaxCheckInput {
		return nil, fail(KindInvalidArgs, "check input is larger than %d bytes", MaxCheckInput), -1
	}
	tmp, err := os.MkdirTemp(r.cfg.TmpDir, "check-"+safeID(req.CallID)+"-")
	if err != nil {
		return nil, fail(KindDenied, "prepare temp dir: %v", err), -1
	}
	defer os.RemoveAll(tmp)
	env, cerr := r.env(t, req.CallID, tmp)
	if cerr != nil {
		return nil, cerr, -1
	}
	out, cerr, code := r.exec(ctx, t, t.spec.Exec, env, req.CallID, input)
	if cerr != nil {
		return nil, cerr, code
	}
	res, err := parseCheckResult(out)
	if err != nil {
		return nil, fail(KindBadOutput, "%v", err), code
	}
	return res, nil, code
}

// parseCheckResult insists on a boolean pass and a string reason: a check
// that prints something else has not decided anything.
func parseCheckResult(out json.RawMessage) (*CheckResult, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(out, &shape); err != nil {
		return nil, fmt.Errorf("check output is not a JSON object")
	}
	var res CheckResult
	if err := json.Unmarshal(shape["pass"], &res.Pass); err != nil || shape["pass"] == nil {
		return nil, fmt.Errorf(`check output needs a boolean "pass"`)
	}
	if err := json.Unmarshal(shape["reason"], &res.Reason); err != nil || shape["reason"] == nil {
		return nil, fmt.Errorf(`check output needs a string "reason"`)
	}
	res.Details, res.Evidence = shape["details"], shape["evidence"]
	return &res, nil
}

func (r *Runner) auditCheck(req CheckRequest, resp CheckResponse, exitCode int) {
	sum := sha256.Sum256([]byte(req.Output))
	attrs := []any{
		"event", "check_call",
		"agent", r.agent.Metadata.Name,
		"check", req.Check,
		"call_id", req.CallID,
		"attempt", req.Attempt,
		"output_sha256", hex.EncodeToString(sum[:]),
		"duration_ms", resp.DurationMS,
		"ok", resp.OK,
		"exit_code", exitCode,
	}
	if resp.Result != nil {
		attrs = append(attrs, "pass", resp.Result.Pass)
	}
	if resp.Error != nil {
		attrs = append(attrs, "error_kind", resp.Error.Kind)
	}
	r.cfg.Logger.Info("audit", attrs...)
}
