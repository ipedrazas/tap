package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipedrazas/tap/pkg/harness"
	"github.com/ipedrazas/tap/pkg/s3"
)

type fakeObject struct {
	body     []byte
	modified time.Time
}

type fakeRunStore struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	heads   int
}

func (f *fakeRunStore) put(key string, body []byte, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{body, modified}
}

func (f *fakeRunStore) List(_ context.Context, prefix string, max int) ([]s3.Object, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []s3.Object
	for k, o := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3.Object{Key: k, Size: int64(len(o.body)), LastModified: o.modified})
		}
	}
	return out, false, nil
}

func (f *fakeRunStore) Get(_ context.Context, key string, max int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return nil, s3.ErrNotFound
	}
	return o.body, nil
}

func (f *fakeRunStore) Head(_ context.Context, key string, n int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads++
	o, ok := f.objects[key]
	if !ok {
		return nil, s3.ErrNotFound
	}
	return o.body[:min(int(n), len(o.body))], nil
}

func trace(id, who, trigger, prompt string, at time.Time) []byte {
	ms := int64(1200)
	t := harness.Trace{Version: 1, ID: id, Source: "tap", AgentName: "demo-agent", AgentVersion: "0.1.2", Customer: harness.UserKey(who), StartTime: at, EndTime: at.Add(time.Minute),
		Turns: []*harness.TraceTurn{{ID: "t0", Trigger: trigger, StartTime: at, EndTime: at.Add(time.Minute), Prompt: prompt, Steps: []*harness.TraceStep{
			{ID: "t0.s0", Kind: "llm_call", LLMCall: &harness.TraceLLMCall{Model: "m", Tokens: &harness.TraceTokens{Input: 10, Output: 5}, ToolCallIDs: []string{"t0.s1"}}},
			{ID: "t0.s1", Index: 1, Kind: "tool_call", ToolCall: &harness.TraceToolCall{Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), Result: `{"answer":"<script>alert(1)</script>"}`, Status: "ok", LatencyMs: &ms}},
			{ID: "t0.s2", Index: 2, Kind: "llm_call", LLMCall: &harness.TraceLLMCall{Model: "m", Text: "All good."}},
		}}}}
	b, _ := json.Marshal(t)
	return b
}

func TestParseHead(t *testing.T) {
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	full := trace("aa", "hook:prs", "hook", "Review https://github.com/o/r/pull/1", at)
	var s runSummary
	parseHead(full, &s)
	if s.Customer != harness.UserKey("hook:prs") || s.Trigger != "hook" || s.Prompt != "Review https://github.com/o/r/pull/1" || !s.Started.Equal(at) || s.Version != "0.1.2" {
		t.Errorf("full: %+v", s)
	}
	// Cut inside the prompt: everything before it survives.
	cut := full[:strings.Index(string(full), "pull/1")]
	s = runSummary{}
	parseHead(cut, &s)
	if s.Trigger != "hook" || s.Prompt != "" || s.Customer == "" {
		t.Errorf("cut: %+v", s)
	}
	s = runSummary{}
	parseHead([]byte("not json"), &s)
	if s != (runSummary{}) {
		t.Errorf("garbage: %+v", s)
	}
}

func TestRuns(t *testing.T) {
	e := newSettingsEnv(t, nil)
	if code, body := e.do(t, "POST", "/api/agents/demo-agent/hooks", "alice@example.com", `{"name":"prs","auth":"github","enabled":true,"prompt":"Review {{.payload.number}}"}`); code != 201 {
		t.Fatalf("create hook: %d %s", code, body)
	}
	base := time.Now().Add(-time.Hour)
	for i := range 30 {
		who, trigger := "alice@example.com", "user"
		if i%2 == 1 {
			who, trigger = "hook:prs", "hook"
		}
		id := fmt.Sprintf("%032x", i+1)
		e.runs.put("demo-agent/"+id+".trace.json", trace(id, who, trigger, fmt.Sprintf("message %d", i), base.Add(time.Duration(i)*time.Minute)), base.Add(time.Duration(i)*time.Minute))
	}
	e.runs.put("demo-agent/notes.txt", []byte("x"), base)
	e.runs.put("other-agent/"+strings.Repeat("f", 32)+".trace.json", trace(strings.Repeat("f", 32), "bob@example.com", "user", "elsewhere", base), base.Add(2*time.Hour))

	if code, _ := e.do(t, "GET", "/agents/demo-agent/runs", "bob@example.com", ""); code != 403 {
		t.Errorf("non-member list: %d", code)
	}
	if code, _ := e.do(t, "GET", "/agents/demo-agent/runs/"+fmt.Sprintf("%032x", 1), "", ""); code != 403 {
		t.Errorf("anonymous run: %d", code)
	}

	code, body := e.do(t, "GET", "/agents/demo-agent/runs", "alice@example.com", "")
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	// Newest first, 25 a page, the viewer and the hook named, other agents
	// and stray objects left out.
	if i29, i5 := strings.Index(body, "message 29"), strings.Index(body, "message 5<"); i29 < 0 || i5 < 0 || i29 > i5 {
		t.Errorf("order: 29 at %d, 5 at %d", i29, i5)
	}
	if strings.Contains(body, "message 4<") || !strings.Contains(body, "?page=1") || strings.Contains(body, "elsewhere") || strings.Contains(body, "notes") {
		t.Error("page 0 holds the wrong runs")
	}
	if !strings.Contains(body, ">you<") || !strings.Contains(body, "hook prs") {
		t.Error("callers not named")
	}
	heads := e.runs.heads
	e.do(t, "GET", "/agents/demo-agent/runs", "alice@example.com", "")
	if e.runs.heads != heads {
		t.Errorf("unchanged runs read again: %d heads", e.runs.heads-heads)
	}
	_, body = e.do(t, "GET", "/agents/demo-agent/runs?page=1", "alice@example.com", "")
	if !strings.Contains(body, "message 4<") || !strings.Contains(body, "message 0<") || strings.Contains(body, "message 5<") {
		t.Error("page 1 holds the wrong runs")
	}

	id := fmt.Sprintf("%032x", 2)
	code, body = e.do(t, "GET", "/agents/demo-agent/runs/"+id, "alice@example.com", "")
	if code != 200 {
		t.Fatalf("run: %d %s", code, body)
	}
	for _, want := range []string{"hook prs", "message 1", "lookup", "All good.", "1.2s", "10 in · 5 out", "&lt;script&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("tool result not escaped")
	}
	code, body = e.do(t, "GET", "/agents/demo-agent/runs/"+id+".json", "alice@example.com", "")
	if code != 200 || !strings.Contains(body, `"session_id":"`+id+`"`) {
		t.Errorf("json: %d", code)
	}
	if code, _ := e.do(t, "GET", "/agents/demo-agent/runs/"+strings.Repeat("e", 32), "alice@example.com", ""); code != 404 {
		t.Errorf("missing run: %d", code)
	}
	if code, _ := e.do(t, "GET", "/agents/demo-agent/runs/..%2fother-agent%2f"+strings.Repeat("f", 32), "alice@example.com", ""); code != 404 {
		t.Errorf("traversal: %d", code)
	}

	_, body = e.do(t, "GET", "/agents/demo-agent", "alice@example.com", "")
	if !strings.Contains(body, `href="demo-agent/runs"`) {
		t.Error("agent page has no runs link")
	}
}
