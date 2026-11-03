package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ipedrazas/tap/pkg/harness"
	"github.com/ipedrazas/tap/pkg/verify"
)

func (f *fakeRunStore) Put(_ context.Context, key, _ string, body []byte) error {
	f.put(key, body, time.Now())
	return nil
}

func report(session string, attempt int, status string, final, review bool) *verify.Report {
	r := &verify.Report{Schema: verify.ReportSchema, Agent: "demo-agent", SessionID: session, TurnID: "t0", Trigger: "hook",
		Attempt: attempt, MaxAttempts: 2, Status: status, Final: final, NeedsReview: review, Task: "Research " + session[:4],
		EndTime: time.Now(), Criteria: []verify.CriterionResult{
			{ID: "links-resolve", Kind: "deterministic", Blocking: true, Status: map[bool]string{true: verify.Pass, false: verify.Fail}[status == verify.StatusPassed], Reason: "[2] https://stats.example.org/x (soft 404)"},
			{ID: "human-sample", Kind: "human", Status: verify.Flagged, Reason: "sampled"},
		}}
	return r
}

// verifiedRun stores a trace and its report file for one session.
func verifiedRun(e *settingsEnv, session string, reports ...*verify.Report) {
	at := time.Now().Add(-time.Minute)
	var tr harness.Trace
	_ = json.Unmarshal(trace(session, "hook:prs", "hook", "Research", at), &tr)
	var lines []string
	for _, r := range reports {
		c := *r
		c.Task, c.Output = "", ""
		tr.Turns[0].Verification = append(tr.Turns[0].Verification, &c)
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	b, _ := json.Marshal(tr)
	e.runs.put("demo-agent/"+session+".trace.json", b, at)
	e.runs.put("demo-agent/"+session+".verify.jsonl", []byte(strings.Join(lines, "\n")+"\n{partial"), at)
}

func TestReviewQueueAndVerdict(t *testing.T) {
	e := newSettingsEnv(t, nil)
	failed := strings.Repeat("a", 32)
	sampled := strings.Repeat("b", 32)
	quiet := strings.Repeat("c", 32)
	verifiedRun(e, failed, report(failed, 1, verify.StatusFailed, false, false), report(failed, 2, verify.StatusFailed, true, true))
	verifiedRun(e, sampled, report(sampled, 1, verify.StatusPassed, true, true))
	verifiedRun(e, quiet, report(quiet, 1, verify.StatusPassed, true, false))

	if code, _ := e.do(t, "GET", "/agents/demo-agent/review", "bob@example.com", ""); code != 403 {
		t.Fatalf("non-member queue: %d", code)
	}
	code, body := e.do(t, "GET", "/agents/demo-agent/review", "alice@example.com", "")
	if code != 200 {
		t.Fatalf("queue: %d %s", code, body)
	}
	if !strings.Contains(body, "runs/"+failed+"#v-t0-2") || !strings.Contains(body, "runs/"+sampled) || strings.Contains(body, quiet) {
		t.Fatalf("queue lists the wrong answers:\n%s", body)
	}
	if !strings.Contains(body, "failed links-resolve") {
		t.Error("queue does not say why")
	}

	// The run page shows each attempt's report and the review form.
	code, page := e.do(t, "GET", "/agents/demo-agent/runs/"+failed, "alice@example.com", "")
	if code != 200 {
		t.Fatalf("run page: %d %s", code, page)
	}
	for _, want := range []string{"attempt 1 of 2", "attempt 2 of 2", "needs review", "soft 404", `data-attempt="2"`, "Save review"} {
		if !strings.Contains(page, want) {
			t.Errorf("run page lacks %q", want)
		}
	}

	path := "/api/agents/demo-agent/runs/" + failed + "/reviews"
	for name, tc := range map[string]struct {
		who, body string
		code      int
	}{
		"non-member":        {"bob@example.com", `{"turn_id":"t0","attempt":2,"verdict":"pass"}`, 403},
		"bad verdict":       {"alice@example.com", `{"turn_id":"t0","attempt":2,"verdict":"maybe"}`, 422},
		"unknown criterion": {"alice@example.com", `{"turn_id":"t0","attempt":2,"verdict":"pass","criteria":{"made-up":"pass"}}`, 422},
		"no such attempt":   {"alice@example.com", `{"turn_id":"t0","attempt":3,"verdict":"pass"}`, 404},
		"bad turn":          {"alice@example.com", `{"turn_id":"../x","attempt":1,"verdict":"pass"}`, 400},
		"unknown field":     {"alice@example.com", `{"turn_id":"t0","attempt":2,"verdict":"pass","reviewer":"mallory"}`, 400},
	} {
		if code, body := e.do(t, "POST", path, tc.who, tc.body); code != tc.code {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	code, body = e.do(t, "POST", path, "alice@example.com", `{"turn_id":"t0","attempt":2,"verdict":"pass","criteria":{"links-resolve":"pass"},"note":"  the page moved, the new URL says the same  "}`)
	if code != 201 {
		t.Fatalf("review: %d %s", code, body)
	}
	if len(e.reviews.objects) != 1 {
		t.Fatalf("reviews bucket: %v", e.reviews.objects)
	}
	for key, o := range e.reviews.objects {
		if !strings.HasPrefix(key, "demo-agent/"+failed+"/t0-2-") {
			t.Errorf("key %s", key)
		}
		var rv verify.Review
		if err := json.Unmarshal(o.body, &rv); err != nil {
			t.Fatal(err)
		}
		// The reviewer is who signed in, not what the body says; the
		// verifier's verdict sits next to the person's.
		if rv.Schema != verify.ReviewSchema || rv.Reviewer != "alice@example.com" || rv.Verdict != "pass" || rv.VerifierStatus != verify.StatusFailed ||
			rv.Criteria["links-resolve"] != "pass" || rv.Note != "the page moved, the new URL says the same" {
			t.Errorf("review %+v", rv)
		}
	}
	// Nothing was written to the sessions bucket.
	for key := range e.runs.objects {
		if strings.Contains(key, "review") {
			t.Errorf("sessions bucket got %s", key)
		}
	}
	// Reviewed answers leave the queue; the run page shows the review.
	_, body = e.do(t, "GET", "/agents/demo-agent/review", "alice@example.com", "")
	if strings.Contains(body, "runs/"+failed) || !strings.Contains(body, "runs/"+sampled) {
		t.Fatalf("queue after review:\n%s", body)
	}
	_, page = e.do(t, "GET", "/agents/demo-agent/runs/"+failed, "alice@example.com", "")
	if !strings.Contains(page, "alice@example.com") || !strings.Contains(page, "the page moved") {
		t.Error("run page does not show the review")
	}
}
