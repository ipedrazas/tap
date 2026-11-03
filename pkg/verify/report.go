// Package verify checks an agent's answer against its acceptance spec,
// outside the conversation that produced it. It sees only the task (the
// turn's input), the answer and the spec. Deterministic checks run first, in
// the runner; a judged criterion runs once the checks it uses (whose
// evidence it reads) have passed, each as its own model call with no tools;
// human criteria flag a sample of runs for review and never block.
//
// The Report is the stable record of one verification (one attempt), written
// as one JSON line to <session>.verify.jsonl. docs/acceptance.md documents
// the schema; change it only by adding fields, and bump ReportSchema when a
// field's meaning changes.
package verify

import (
	"encoding/json"
	"time"
)

// ReportSchema names the version of Report and CriterionResult.
const ReportSchema = "tap.verification.v1"

// ReviewSchema names the version of Review.
const ReviewSchema = "tap.review.v1"

// Statuses of a whole report.
const (
	StatusPassed = "passed" // every blocking criterion passed
	StatusFailed = "failed" // a blocking criterion failed or errored
)

// Statuses of one criterion.
const (
	Pass       = "pass"
	Fail       = "fail"
	Error      = "error"       // the check or judge could not decide; counts as a failure
	Skipped    = "skipped"     // judged criteria whose used checks did not pass
	Flagged    = "flagged"     // human: sampled for review
	NotSampled = "not_sampled" // human: not sampled this time
)

type Report struct {
	Schema       string `json:"schema"`
	Agent        string `json:"agent"`
	AgentVersion string `json:"agent_version"`
	// SpecHash identifies the acceptance spec (sha256 of acceptance.yaml as JSON).
	SpecHash  string `json:"spec_hash"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Trigger   string `json:"trigger"` // chat | hook
	Attempt   int    `json:"attempt"`
	// MaxAttempts is how many attempts this turn was allowed.
	MaxAttempts int    `json:"max_attempts"`
	Status      string `json:"status"`
	// Final marks the last attempt of the turn: its status is the turn's.
	Final bool `json:"final"`
	// NeedsReview is set on a final report sampled by a human criterion.
	NeedsReview bool              `json:"needs_review"`
	Task        string            `json:"task"`
	Output      string            `json:"output"`
	Truncated   bool              `json:"truncated,omitempty"`
	StartTime   time.Time         `json:"start_time"`
	EndTime     time.Time         `json:"end_time"`
	DurationMS  int64             `json:"duration_ms"`
	Criteria    []CriterionResult `json:"criteria"`
}

type CriterionResult struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Blocking   bool            `json:"blocking"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason"`
	Score      *float64        `json:"score,omitempty"`
	Threshold  *float64        `json:"threshold,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
	StartTime  time.Time       `json:"start_time,omitzero"`
	DurationMS int64           `json:"duration_ms"`
}

// Passed reports whether every blocking criterion passed.
func (r *Report) Passed() bool { return r.Status == StatusPassed }

// Failures returns the blocking criteria that failed or errored (not the
// judged ones skipped because a check they use failed).
func (r *Report) Failures() []CriterionResult {
	var out []CriterionResult
	for _, c := range r.Criteria {
		if c.Blocking && (c.Status == Fail || c.Status == Error) {
			out = append(out, c)
		}
	}
	return out
}

// Review is a person's verdict on one verified answer, recorded by the
// console: the override of the verifier, and the label for the dataset.
type Review struct {
	Schema    string    `json:"schema"`
	Agent     string    `json:"agent"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	Attempt   int       `json:"attempt"`
	Reviewer  string    `json:"reviewer"`
	Time      time.Time `json:"time"`
	// Verdict is the person's judgement of the answer: pass or fail.
	Verdict string `json:"verdict"`
	// VerifierStatus is what the verifier said, so a disagreement is a
	// single comparison when the dataset is read.
	VerifierStatus string `json:"verifier_status"`
	// Criteria overrides individual criteria: id -> pass | fail.
	Criteria map[string]string `json:"criteria,omitempty"`
	Note     string            `json:"note,omitempty"`
}
