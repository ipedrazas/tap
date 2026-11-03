package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ipedrazas/tap/pkg/verify"
)

// TurnInfo names the turn being verified, for the report.
type TurnInfo struct {
	SessionID, TurnID string
	// Trigger is "user" (chat) or "hook", as runTurn has it.
	Trigger string
}

// VerifiedTurn runs one turn and, when the bundle's acceptance spec covers
// this trigger, verifies the answer outside the conversation. A failed
// attempt sends the agent the failures (verify.Feedback) as a new message in
// the same session and verifies its next answer, up to the spec's
// maxAttempts. record receives every report; the last has Final set and
// carries the turn's outcome. Without a spec this is exactly Turn.
func (a *Agent) VerifiedTurn(ctx context.Context, history []Message, user string, info TurnInfo, rec *TraceTurn, emit func(Event), record func(*verify.Report)) ([]Message, error) {
	history, err := a.Turn(ctx, history, user, rec, emit)
	acc := a.Bundle.Acceptance
	if err != nil || a.Verifier == nil || !acc.VerifiesOn(info.Trigger) {
		return history, err
	}
	trigger := info.Trigger
	if trigger == "user" {
		trigger = "chat"
	}
	maxAttempts := acc.Attempts()
	for attempt := 1; ; attempt++ {
		emit(Event{Type: "verifying", Content: fmt.Sprintf("verifying the answer (attempt %d of %d)", attempt, maxAttempts)})
		rep := a.Verifier.Verify(ctx, verify.Input{
			SessionID: info.SessionID, TurnID: info.TurnID, Trigger: trigger,
			Task: user, Output: lastAnswer(history), Attempt: attempt,
		})
		rep.Final = rep.Passed() || attempt >= maxAttempts || ctx.Err() != nil
		// A person looks at sampled answers, and at every answer that ran
		// out of attempts: those are where the criteria are most likely wrong.
		rep.NeedsReview = rep.Final && (!rep.Passed() || flagged(rep))
		a.Logger.Info("verification", "session", info.SessionID, "turn", info.TurnID, "attempt", attempt,
			"status", rep.Status, "final", rep.Final, "needs_review", rep.NeedsReview, "failures", failureIDs(rep))
		record(rep)
		emit(Event{Type: "verification", Content: summary(rep), Verification: rep})
		if rep.Final {
			return history, nil
		}
		if history, err = a.Turn(ctx, history, verify.Feedback(rep), rec, emit); err != nil {
			return history, err
		}
	}
}

// lastAnswer is the content of the last assistant message: the answer, or
// what the agent had said when it ran out of model turns.
func lastAnswer(history []Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if m := history[i]; m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

func flagged(r *verify.Report) bool {
	for _, c := range r.Criteria {
		if c.Status == verify.Flagged {
			return true
		}
	}
	return false
}

func failureIDs(r *verify.Report) []string {
	var ids []string
	for _, c := range r.Failures() {
		ids = append(ids, c.ID)
	}
	return ids
}

func summary(r *verify.Report) string {
	switch {
	case r.Passed():
		return fmt.Sprintf("verified: every blocking criterion passed (attempt %d)", r.Attempt)
	case r.Final:
		return fmt.Sprintf("not verified after %d attempt(s): %s failed; this answer is marked failed", r.Attempt, strings.Join(failureIDs(r), ", "))
	}
	return fmt.Sprintf("attempt %d failed verification (%s); asking the agent to fix it", r.Attempt, strings.Join(failureIDs(r), ", "))
}

// NewVerifier builds the verifier for b: deterministic checks through the
// runner, judged criteria through judge (a gateway route of their own).
func NewVerifier(b *Bundle, judge Model, runner *RunnerClient) *verify.Verifier {
	if b.Acceptance == nil {
		return nil
	}
	return &verify.Verifier{
		Spec: b.Acceptance, Rubrics: b.Rubrics,
		Checker: runner, Judge: ModelJudge{Model: judge},
		Agent: b.Agent.Metadata.Name, Version: b.Agent.Metadata.Version,
	}
}

// ModelJudge makes a single model call with no tools and a fresh message
// list: nothing of the producing conversation reaches it.
type ModelJudge struct{ Model Model }

func (j ModelJudge) Judge(ctx context.Context, system, user string) (string, error) {
	c, err := j.Model.Complete(ctx, []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, nil)
	if err != nil {
		return "", err
	}
	return c.Message.Content, nil
}

// Check runs one deterministic check on the runner (POST /v1/check).
func (r *RunnerClient) Check(ctx context.Context, id, task, output string, attempt int, callID string) (*verify.CheckResult, error) {
	body, _ := json.Marshal(map[string]any{"check": id, "task": task, "output": output, "attempt": attempt, "call_id": callID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/v1/check", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner: %s", resp.Status)
	}
	var cr struct {
		OK     bool                `json:"ok"`
		Result *verify.CheckResult `json:"result"`
		Error  *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	switch {
	case cr.OK && cr.Result != nil:
		return cr.Result, nil
	case cr.Error != nil:
		return nil, fmt.Errorf("%s: %s", cr.Error.Kind, cr.Error.Message)
	}
	return nil, fmt.Errorf("runner: check response without a result")
}
