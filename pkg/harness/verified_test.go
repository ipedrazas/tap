package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipedrazas/tap/pkg/verify"
)

const acceptanceYAML = `apiVersion: tavon.ai/acceptance/v1
kind: Acceptance
verifier: { model: judge }
criteria:
  - { id: cited, description: every claim is cited, kind: deterministic, exec: [node, checks/cited.ts] }
  - { id: on-topic, description: answers the question, kind: judged, rubricFile: acceptance/on-topic.md, threshold: 0.5 }
`

func acceptanceBundle(t *testing.T, acc string) string {
	t.Helper()
	dir := bundleDir(t)
	for name, body := range map[string]string{
		"acceptance.yaml":        acc,
		"acceptance/on-topic.md": "SECRET-RUBRIC: score 1 when the answer is about the question.",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// answers replies with each answer in turn, recording what it saw.
type answers struct {
	replies []string
	seen    [][]Message
}

func (m *answers) Complete(_ context.Context, msgs []Message, tools []ToolDef) (Completion, error) {
	m.seen = append(m.seen, msgs)
	r := m.replies[0]
	if len(m.replies) > 1 {
		m.replies = m.replies[1:]
	}
	return Completion{Message: Message{Role: "assistant", Content: r}, FinishReason: "stop"}, nil
}

// citationRunner fails "cited" for any answer without "[1]".
func citationRunner(t *testing.T, calls *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/check" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		*calls = append(*calls, req)
		pass := strings.Contains(req["output"].(string), "[1]")
		reason := "the claim has no citation"
		if pass {
			reason = "1 claim, 1 cited"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"pass": pass, "reason": reason}})
	}))
}

func verifiedAgent(t *testing.T, dir string, producer, judge Model, runnerURL string) *Agent {
	t.Helper()
	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	rc := &RunnerClient{URL: runnerURL, Token: "tok", Client: http.DefaultClient}
	return &Agent{Bundle: b, Model: producer, Runner: rc, Verifier: NewVerifier(b, judge, rc), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestVerifiedTurnRetriesWithFeedback(t *testing.T) {
	var checks []map[string]any
	runner := citationRunner(t, &checks)
	defer runner.Close()
	producer := &answers{replies: []string{"Paris is the capital.", "Paris is the capital [1]."}}
	judge := &answers{replies: []string{`{"score": 0.9, "reason": "on topic"}`}}
	a := verifiedAgent(t, acceptanceBundle(t, acceptanceYAML), producer, judge, runner.URL)

	var reports []*verify.Report
	var events []Event
	history, err := a.VerifiedTurn(context.Background(), nil, "What is the capital of France?", TurnInfo{SessionID: "s1", TurnID: "t0", Trigger: "user"}, nil,
		func(e Event) { events = append(events, e) }, func(r *verify.Report) { reports = append(reports, r) })
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("want 2 reports, got %d", len(reports))
	}
	first, second := reports[0], reports[1]
	if first.Passed() || first.Final || first.Attempt != 1 || second.Attempt != 2 || !second.Passed() || !second.Final {
		t.Fatalf("attempts: %+v / %+v", first, second)
	}
	if first.Trigger != "chat" || first.Task != "What is the capital of France?" || first.Output != "Paris is the capital." {
		t.Fatalf("first report: %+v", first)
	}
	// Judged criteria only ran once the citations were there.
	if len(judge.seen) != 1 {
		t.Fatalf("judge called %d times; want once (attempt 2 only)", len(judge.seen))
	}
	// The judge has its own two-message context: nothing of the producer's.
	if js := judge.seen[0]; len(js) != 2 || js[0].Role != "system" || !strings.Contains(js[0].Content, "SECRET-RUBRIC") ||
		strings.Contains(js[0].Content, "You are a unit test") {
		t.Fatalf("judge context: %+v", js)
	}
	// The producer got the failure, and never the rubric.
	var feedback string
	for _, m := range history {
		if m.Role == "user" && strings.HasPrefix(m.Content, "Your previous answer did not pass verification") {
			feedback = m.Content
		}
		if strings.Contains(m.Content, "SECRET-RUBRIC") {
			t.Fatal("the rubric reached the producing conversation")
		}
	}
	if !strings.Contains(feedback, "cited (fail): the claim has no citation") {
		t.Fatalf("feedback: %q", feedback)
	}
	if lastAnswer(history) != "Paris is the capital [1]." || len(checks) != 2 {
		t.Fatalf("answer %q, checks %d", lastAnswer(history), len(checks))
	}
	var verdicts int
	for _, e := range events {
		if e.Type == "verification" {
			verdicts++
		}
	}
	if verdicts != 2 {
		t.Fatalf("want 2 verification events, got %d", verdicts)
	}
}

func TestVerifiedTurnGivesUpAfterMaxAttempts(t *testing.T) {
	var checks []map[string]any
	runner := citationRunner(t, &checks)
	defer runner.Close()
	producer := &answers{replies: []string{"Never cited."}}
	judge := &answers{replies: []string{`{"score": 1, "reason": "x"}`}}
	acc := strings.Replace(acceptanceYAML, "kind: Acceptance\n", "kind: Acceptance\nmaxAttempts: 3\n", 1)
	a := verifiedAgent(t, acceptanceBundle(t, acc), producer, judge, runner.URL)
	var reports []*verify.Report
	_, err := a.VerifiedTurn(context.Background(), nil, "q", TurnInfo{SessionID: "s", TurnID: "t0", Trigger: "hook"}, nil, func(Event) {}, func(r *verify.Report) { reports = append(reports, r) })
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 {
		t.Fatalf("want 3 attempts, got %d", len(reports))
	}
	last := reports[2]
	if last.Passed() || !last.Final || !last.NeedsReview || last.MaxAttempts != 3 {
		t.Fatalf("last report: %+v", last)
	}
	if len(judge.seen) != 0 {
		t.Fatal("judge ran although a blocking check failed")
	}
}

func TestNoSpecIsPlainTurn(t *testing.T) {
	b, err := LoadBundle(bundleDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if b.Acceptance != nil || NewVerifier(b, nil, nil) != nil {
		t.Fatal("a bundle without acceptance.yaml has a verifier")
	}
	producer := &answers{replies: []string{"hello"}}
	a := &Agent{Bundle: b, Model: producer, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	called := false
	history, err := a.VerifiedTurn(context.Background(), nil, "hi", TurnInfo{Trigger: "user"}, nil, func(Event) {}, func(*verify.Report) { called = true })
	if err != nil || called || len(history) != 3 {
		t.Fatalf("err %v, recorded %v, history %d", err, called, len(history))
	}
}

func TestVerifyOnSkipsChat(t *testing.T) {
	acc := strings.Replace(acceptanceYAML, "kind: Acceptance\n", "kind: Acceptance\nverifyOn: [hook]\n", 1)
	a := verifiedAgent(t, acceptanceBundle(t, acc), &answers{replies: []string{"x"}}, &answers{replies: []string{"{}"}}, "http://127.0.0.1:1")
	called := false
	if _, err := a.VerifiedTurn(context.Background(), nil, "hi", TurnInfo{Trigger: "user"}, nil, func(Event) {}, func(*verify.Report) { called = true }); err != nil || called {
		t.Fatalf("chat turn verified (err %v)", err)
	}
}

// runTurn writes each report as a JSONL line and into the trace.
func TestReportsPersisted(t *testing.T) {
	var checks []map[string]any
	runner := citationRunner(t, &checks)
	defer runner.Close()
	producer := &answers{replies: []string{"uncited", "cited [1]"}}
	judge := &answers{replies: []string{`{"score": 0.9, "reason": "on topic"}`}}
	a := verifiedAgent(t, acceptanceBundle(t, acceptanceYAML), producer, judge, runner.URL)
	traces := t.TempDir()
	s := &Server{Agent: a, Workspace: t.TempDir(), TracesDir: traces, Logger: a.Logger}
	sess := s.session("hook:x", "0123456789abcdef0123456789abcdef")
	if err := s.runTurn(context.Background(), "hook:x", "0123456789abcdef0123456789abcdef", "hook", sess, "q", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(traces, "0123456789abcdef0123456789abcdef.verify.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []verify.Report
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r verify.Report
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, r)
	}
	if len(lines) != 2 || lines[0].Schema != verify.ReportSchema || lines[1].Status != verify.StatusPassed || lines[0].SessionID != "0123456789abcdef0123456789abcdef" || lines[0].TurnID != "t0" {
		t.Fatalf("reports: %+v", lines)
	}
	data, err := os.ReadFile(filepath.Join(traces, "0123456789abcdef0123456789abcdef.trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tr Trace
	if err := json.Unmarshal(data, &tr); err != nil {
		t.Fatal(err)
	}
	if v := tr.Turns[0].Verification; len(v) != 2 || v[1].Output != "" || !v[1].Final {
		t.Fatalf("trace verification: %+v", v)
	}
}
