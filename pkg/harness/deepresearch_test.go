package harness_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/harness"
	"github.com/ipedrazas/tap/pkg/runner"
	"github.com/ipedrazas/tap/pkg/spec"
	"github.com/ipedrazas/tap/pkg/verify"
)

// The deep-research reference agent, end to end: its real tools and checks
// run in the real runner (node), the web is the egress mock, the producing
// model cites a page that is a soft 404, links-resolve must catch it, the
// agent gets the reason and fixes its report, and both verification reports
// land in <session>.verify.jsonl and the trace.

const bundle = "../../testdata/agents/deep-research-agent"

const (
	badReport = `# Population of Paris

The City of Paris has an estimated population of 2,102,650 residents as of January 2023 [1].
Paris had about 2.2 million residents in 2019 [2].

## Sources

1. Paris - Wikipedia — https://en.wikipedia.org/wiki/Paris
2. Paris population statistics 2019 — https://stats.example.org/paris/2019
`
	goodReport = `# Population of Paris

The City of Paris has an estimated population of 2,102,650 residents as of January 2023 [1].
> Note: I could not find a working source for the 2019 figure, so it is left out.

## Sources

1. Paris - Wikipedia — https://en.wikipedia.org/wiki/Paris
`
)

// researcher searches, reads the good page, answers with a bad citation,
// and fixes the report when verification tells it what failed.
type researcher struct {
	mu   sync.Mutex
	step int
	seen []harness.Message
}

func (m *researcher) Complete(_ context.Context, msgs []harness.Message, tools []harness.ToolDef) (harness.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := msgs[len(msgs)-1]
	m.seen = append(m.seen, last)
	tc := func(id, name, args string) harness.Completion {
		return harness.Completion{Message: harness.Message{Role: "assistant", ToolCalls: []harness.ToolCall{{ID: id, Type: "function", Function: harness.FunctionCall{Name: name, Arguments: args}}}}}
	}
	answer := func(s string) harness.Completion {
		return harness.Completion{Message: harness.Message{Role: "assistant", Content: s}, FinishReason: "stop"}
	}
	if last.Role == "user" && strings.HasPrefix(last.Content, "Your previous answer did not pass verification") {
		return answer(goodReport), nil
	}
	m.step++
	switch m.step {
	case 1:
		return tc("c1", "web_search", `{"query":"population of Paris"}`), nil
	case 2:
		return tc("c2", "fetch_page", `{"url":"https://en.wikipedia.org/wiki/Paris"}`), nil
	}
	return answer(badReport), nil
}

// judge is the verifier model: it passes only when the evidence it was
// given contains the page that supports the claim.
type judge struct {
	mu    sync.Mutex
	users []string
}

func (j *judge) Complete(_ context.Context, msgs []harness.Message, tools []harness.ToolDef) (harness.Completion, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(msgs) != 2 || tools != nil {
		return harness.Completion{}, errUnexpected
	}
	j.users = append(j.users, msgs[1].Content)
	reply := `{"score": 0.2, "reason": "no evidence"}`
	if strings.Contains(msgs[0].Content, "answers-question") || strings.Contains(msgs[1].Content, "2,102,650 residents as of January 2023") {
		reply = `{"score": 0.95, "reason": "supported by the cited page"}`
	}
	return harness.Completion{Message: harness.Message{Role: "assistant", Content: reply}}, nil
}

var errUnexpected = &json.UnsupportedValueError{Str: "judge got a conversation, not a fresh two-message call"}

func TestDeepResearchCatchesBadCitation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	// Version managers put shims on PATH that need their own environment.
	if out, err := exec.Command(node, "-p", "process.execPath").Output(); err == nil {
		node = strings.TrimSpace(string(out))
	}
	tmp := t.TempDir()
	mock, err := egress.StartMock(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	var web []spec.RecordedHTTP
	data, err := os.ReadFile("testdata/deep-research-web.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &web); err != nil {
		t.Fatal(err)
	}
	mock.Prepare([]string{"*:443", "api.exa.ai:443"}, web)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := runner.New(runner.Config{
		BundleDir: bundle, Workspace: filepath.Join(tmp, "ws"), TmpDir: tmp, Logger: logger,
		InterpreterPaths: map[string]string{"node": node},
		Secrets:          func(string) (string, error) { return "stub", nil },
		Egress:           func(string, time.Duration) ([]string, error) { return mock.Env(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Checks(), ","); got != "claims-cited,links-resolve" {
		t.Fatalf("runner checks: %s", got)
	}
	rs := httptest.NewServer(runner.Handler(r, "tok", 4))
	defer rs.Close()

	b, err := harness.LoadBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	rc := &harness.RunnerClient{URL: rs.URL, Token: "tok", Client: http.DefaultClient}
	producer, j := &researcher{}, &judge{}
	agent := &harness.Agent{Bundle: b, Model: producer, Runner: rc, Verifier: harness.NewVerifier(b, j, rc), Logger: logger}
	traces := filepath.Join(tmp, "traces")
	srv := &harness.Server{Agent: agent, Workspace: filepath.Join(tmp, "ws"), TracesDir: traces, Logger: logger}

	var events []harness.Event
	if err := harness.RunTurnForTest(srv, "hook:research", "How many people live in Paris, and how many lived there in 2019?", func(e harness.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if p := mock.Problems(); len(p) > 0 {
		t.Fatalf("egress mock: %v", p)
	}

	reports := readReports(t, traces)
	if len(reports) != 2 {
		t.Fatalf("want 2 attempts, got %d: %+v", len(reports), reports)
	}
	first, second := reports[0], reports[1]
	byID := func(r verify.Report) map[string]verify.CriterionResult {
		m := map[string]verify.CriterionResult{}
		for _, c := range r.Criteria {
			m[c.ID] = c
		}
		return m
	}
	c1 := byID(first)
	if first.Status != verify.StatusFailed || first.Final || first.Trigger != "hook" {
		t.Fatalf("attempt 1: %+v", first)
	}
	if c1["claims-cited"].Status != verify.Pass {
		t.Fatalf("claims-cited on attempt 1: %+v", c1["claims-cited"])
	}
	// The deliberately bad citation is caught, with the URL and why.
	lr := c1["links-resolve"]
	if lr.Status != verify.Fail || !strings.Contains(lr.Reason, "https://stats.example.org/paris/2019") || !strings.Contains(lr.Reason, "soft 404") {
		t.Fatalf("links-resolve on attempt 1: %+v", lr)
	}
	// source-supports-claim reads links-resolve's evidence, so it waits for
	// it; answers-question uses no check and is judged on every attempt.
	if c1["source-supports-claim"].Status != verify.Skipped || c1["answers-question"].Status != verify.Pass {
		t.Fatalf("judged criteria on attempt 1: %+v", c1)
	}
	if len(j.users) != 3 {
		t.Fatalf("judge called %d times; want 3 (answers-question on attempt 1, both on attempt 2)", len(j.users))
	}
	// The judge got the page text links-resolve collected.
	if !strings.Contains(strings.Join(j.users, "\n"), "2,102,650 residents as of January 2023") {
		t.Fatal("source-supports-claim did not get the page evidence")
	}
	// The agent was told what failed, and only that.
	var feedback string
	for _, m := range producer.seen {
		if strings.HasPrefix(m.Content, "Your previous answer did not pass verification") {
			feedback = m.Content
		}
	}
	if !strings.Contains(feedback, "links-resolve (fail)") || !strings.Contains(feedback, "stats.example.org/paris/2019") || strings.Contains(feedback, "Score =") {
		t.Fatalf("feedback: %q", feedback)
	}
	if second.Status != verify.StatusPassed || !second.Final || second.Attempt != 2 || strings.Contains(second.Output, "stats.example.org") {
		t.Fatalf("attempt 2: %+v", second)
	}
	for _, c := range second.Criteria {
		if c.Kind != spec.KindHuman && c.Status != verify.Pass {
			t.Errorf("attempt 2 %s: %s %s", c.ID, c.Status, c.Reason)
		}
		if c.Kind == spec.KindDeterministic && c.DurationMS <= 0 {
			t.Errorf("%s has no timing", c.ID)
		}
	}
	var final *harness.Event
	for i := range events {
		if events[i].Type == "verification" {
			final = &events[i]
		}
	}
	if final == nil || final.Verification.Status != verify.StatusPassed {
		t.Fatalf("final verification event: %+v", final)
	}
}

func readReports(t *testing.T, dir string) []verify.Report {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.verify.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want one report file, got %v", matches)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []verify.Report
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var r verify.Report
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}
