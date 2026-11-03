package verify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ipedrazas/tap/pkg/spec"
)

// CheckResult is what a deterministic check decided (runner.CheckResult).
type CheckResult struct {
	Pass     bool            `json:"pass"`
	Reason   string          `json:"reason"`
	Details  json.RawMessage `json:"details,omitempty"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// Checker runs one deterministic check in the runner. An error means the
// check could not run (runner unavailable, timeout, bad output).
type Checker interface {
	Check(ctx context.Context, id, task, output string, attempt int, callID string) (*CheckResult, error)
}

// Judge is one model call with no tools: a system prompt and a user
// message in, the model's text out. It never shares a conversation with the
// producing agent.
type Judge interface {
	Judge(ctx context.Context, system, user string) (string, error)
}

// Limits on what reaches the judge and the report.
const (
	maxJudgedOutput = 200 << 10
	maxEvidence     = 64 << 10
	maxReportText   = 64 << 10
	maxDetails      = 16 << 10
	maxReason       = 2000
)

type Verifier struct {
	Spec *spec.Acceptance
	// Rubrics holds each judged criterion's rubric text, by id (inline or
	// read from rubricFile when the bundle was loaded).
	Rubrics map[string]string
	Checker Checker
	Judge   Judge
	Agent   string
	Version string
	Now     func() time.Time
}

// Input is one answer to verify.
type Input struct {
	SessionID, TurnID, Trigger string
	Task, Output               string
	Attempt                    int
}

// Verify checks one answer and always returns a report.
func (v *Verifier) Verify(ctx context.Context, in Input) *Report {
	now := v.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	rep := &Report{
		Schema: ReportSchema, Agent: v.Agent, AgentVersion: v.Version, SpecHash: v.specHash(),
		SessionID: in.SessionID, TurnID: in.TurnID, Trigger: in.Trigger,
		Attempt: in.Attempt, MaxAttempts: v.Spec.Attempts(), StartTime: start,
	}
	rep.Task, rep.Truncated = clip(in.Task, maxReportText)
	var cut bool
	rep.Output, cut = clip(in.Output, maxReportText)
	rep.Truncated = rep.Truncated || cut

	evidence := map[string]json.RawMessage{}
	status := map[string]string{}
	// Cheap before expensive: every deterministic check, then judged ones.
	for _, c := range v.Spec.Criteria {
		if c.Kind != spec.KindDeterministic {
			continue
		}
		res := v.deterministic(ctx, c, in, evidence, now)
		status[c.ID] = res.Status
		rep.Criteria = append(rep.Criteria, res)
	}
	for _, c := range v.Spec.Criteria {
		switch c.Kind {
		case spec.KindJudged:
			// A judged criterion waits only for the checks whose evidence it
			// reads. The others don't change its verdict, and judging anyway
			// means one retry can fix every problem, not one stage of them.
			if id, ok := unmet(c, status); !ok {
				rep.Criteria = append(rep.Criteria, CriterionResult{ID: c.ID, Kind: c.Kind, Blocking: c.IsBlocking(), Status: Skipped,
					Reason: fmt.Sprintf("not judged: %s, whose evidence it uses, did not pass", id), Threshold: c.Threshold})
				continue
			}
			rep.Criteria = append(rep.Criteria, v.judged(ctx, c, in, evidence, now))
		case spec.KindHuman:
			res := CriterionResult{ID: c.ID, Kind: c.Kind, Status: NotSampled, Reason: fmt.Sprintf("sampled at %g", c.SampleRate)}
			if Sampled(in.SessionID+"/"+in.TurnID+"/"+c.ID, c.SampleRate) {
				res.Status, res.Reason = Flagged, fmt.Sprintf("sampled for human review (rate %g)", c.SampleRate)
			}
			rep.Criteria = append(rep.Criteria, res)
		}
	}
	rep.Status = StatusPassed
	for _, c := range rep.Criteria {
		if c.Blocking && c.Status != Pass {
			rep.Status = StatusFailed
		}
	}
	rep.EndTime = now()
	rep.DurationMS = rep.EndTime.Sub(start).Milliseconds()
	return rep
}

func (v *Verifier) deterministic(ctx context.Context, c spec.Criterion, in Input, evidence map[string]json.RawMessage, now func() time.Time) CriterionResult {
	t0 := now()
	res := CriterionResult{ID: c.ID, Kind: c.Kind, Blocking: c.IsBlocking(), StartTime: t0}
	callID := fmt.Sprintf("verify-%s-%s-%d-%s", in.SessionID, in.TurnID, in.Attempt, c.ID)
	out, err := v.Checker.Check(ctx, c.ID, in.Task, in.Output, in.Attempt, callID)
	res.DurationMS = now().Sub(t0).Milliseconds()
	if err != nil {
		res.Status, res.Reason = Error, "the check could not run: "+oneLine(err.Error(), maxReason)
		return res
	}
	res.Status = Fail
	if out.Pass {
		res.Status = Pass
	}
	res.Reason, _ = clip(out.Reason, maxReason)
	if len(out.Details) > 0 && len(out.Details) <= maxDetails && json.Valid(out.Details) {
		res.Details = out.Details
	}
	if len(out.Evidence) > 0 && json.Valid(out.Evidence) {
		evidence[c.ID] = out.Evidence
	}
	return res
}

func (v *Verifier) judged(ctx context.Context, c spec.Criterion, in Input, evidence map[string]json.RawMessage, now func() time.Time) CriterionResult {
	t0 := now()
	res := CriterionResult{ID: c.ID, Kind: c.Kind, Blocking: c.IsBlocking(), Threshold: c.Threshold, StartTime: t0}
	threshold := 0.0
	if c.Threshold != nil {
		threshold = *c.Threshold
	}
	system, user := JudgePrompt(c, v.Rubrics[c.ID], in.Task, in.Output, usedEvidence(c, evidence))
	text, err := v.Judge.Judge(ctx, system, user)
	res.DurationMS = now().Sub(t0).Milliseconds()
	if err != nil {
		res.Status, res.Reason = Error, "the verifier model could not be reached: "+oneLine(err.Error(), maxReason)
		return res
	}
	verdict, err := ParseVerdict(text)
	if err != nil {
		res.Status, res.Reason = Error, "the verifier's answer was not a verdict: "+err.Error()
		return res
	}
	score := verdict.Score
	res.Score = &score
	res.Reason, _ = clip(verdict.Reason, maxReason)
	// The threshold decides, not the model's own pass/fail.
	res.Status = Fail
	if score >= threshold {
		res.Status = Pass
	}
	return res
}

// unmet returns the first check in c.Uses that did not pass, and false; or
// "", true when every one passed (or c uses none).
func unmet(c spec.Criterion, status map[string]string) (string, bool) {
	for _, id := range c.Uses {
		if status[id] != Pass {
			return id, false
		}
	}
	return "", true
}

func usedEvidence(c spec.Criterion, evidence map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for _, id := range c.Uses {
		if e, ok := evidence[id]; ok {
			out[id], _ = clip(string(e), maxEvidence)
		}
	}
	return out
}

func (v *Verifier) specHash() string {
	sum := sha256.Sum256(v.Spec.Raw)
	return hex.EncodeToString(sum[:])
}

// Sampled decides human sampling from a key, so the same run is always
// sampled the same way (a retried pod doesn't change the sample).
func Sampled(key string, rate float64) bool {
	sum := sha256.Sum256([]byte(key))
	x := float64(binary.BigEndian.Uint64(sum[:8])>>11) / float64(1<<53)
	return x < rate
}

// Feedback is the message the producing agent gets after a failed attempt:
// the blocking failures, each with its reason and details. It holds no
// rubric text, only what was wrong.
func Feedback(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your previous answer did not pass verification (attempt %d of %d). Fix these problems and give your complete answer again, not only the changes:\n", r.Attempt, r.MaxAttempts)
	for _, c := range r.Failures() {
		fmt.Fprintf(&b, "\n- %s (%s): %s", c.ID, c.Status, c.Reason)
		if len(c.Details) > 0 {
			d, _ := clip(string(c.Details), 4<<10)
			fmt.Fprintf(&b, "\n  details: %s", d)
		}
	}
	return b.String()
}

// nonce makes the tags around untrusted text unguessable, so the text
// cannot close them. A variable for tests.
var nonce = func() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	// Don't cut a UTF-8 sequence in half.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n], true
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	s, _ = clip(s, n)
	return s
}
