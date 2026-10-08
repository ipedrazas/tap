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
)

const agentYAML = `apiVersion: tavon.ai/agent/v1
kind: Agent
metadata: { name: unit, version: 0.0.1, owner: me, description: unit }
harness: { api: v1, model: { provider: gateway, name: sim }, maxTurns: 5 }
effectsPolicy: { write: deny, irreversible: deny }
prompt: system.md
skills: [skills/demo]
runner:
  image: registry.hiddenfield.dev/tap/runner-node@sha256:1111111111111111111111111111111111111111111111111111111111111111
tools:
  - name: lookup
    description: look something up
    input_schema: { type: object, properties: { id: { type: string, pattern: "^[0-9]+$" } }, required: [id], additionalProperties: false }
    exec: ["node", "tools/lookup.ts", "{{id}}"]
    effects: read
  - name: delete_all
    description: delete everything
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["node", "tools/delete.ts"]
    effects: irreversible
`

func bundleDir(t *testing.T) string {
	dir := t.TempDir()
	files := map[string]string{
		"agent.yaml":           agentYAML,
		"system.md":            "You are a unit test.",
		"skills/demo/SKILL.md": "---\nname: demo\ndescription: demo skill\n---\nUse lookup.\n",
		"skills/demo/ref.md":   "reference",
	}
	for name, body := range files {
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

// scriptedModel replays tool calls, then answers with the last tool result.
type scriptedModel struct {
	calls [][]ToolCall
	seen  [][]Message
}

func (m *scriptedModel) Complete(_ context.Context, msgs []Message, _ []ToolDef) (Message, error) {
	m.seen = append(m.seen, msgs)
	if len(m.calls) > 0 {
		next := m.calls[0]
		m.calls = m.calls[1:]
		return Message{Role: "assistant", ToolCalls: next}, nil
	}
	return Message{Role: "assistant", Content: "final: " + msgs[len(msgs)-1].Content}, nil
}

func call(id, name, args string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: args}}
}

func TestTurn(t *testing.T) {
	var runnerCalls []string
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		runnerCalls = append(runnerCalls, req["tool"].(string))
		_, _ = io.WriteString(w, `{"call_id":"x","ok":true,"output":{"found":true}}`)
	}))
	defer runner.Close()

	b, err := LoadBundle(bundleDir(t))
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{calls: [][]ToolCall{
		{call("1", "read_skill_file", `{"path":"skills/demo/ref.md"}`)},
		{call("2", "read_skill_file", `{"path":"skills/../agent.yaml"}`)},
		{call("3", "delete_all", `{}`)},
		{call("4", "lookup", `{"id":"42"}`)},
	}}
	a := &Agent{Bundle: b, Model: model, Runner: &RunnerClient{URL: runner.URL, Token: "tok", Client: http.DefaultClient}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var events []Event
	history, err := a.Turn(context.Background(), nil, "hi", func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(runnerCalls, ",") != "lookup" {
		t.Fatalf("runner saw %v; built-ins and denied tools must not reach it", runnerCalls)
	}
	tool := func(id string) string {
		for _, m := range history {
			if m.ToolCallID == id {
				return m.Content
			}
		}
		return ""
	}
	if !strings.Contains(tool("1"), "reference") {
		t.Errorf("skill read: %s", tool("1"))
	}
	if !strings.Contains(tool("2"), "invalid_args") {
		t.Errorf("path escape not blocked: %s", tool("2"))
	}
	if !strings.Contains(tool("3"), "denied") {
		t.Errorf("irreversible tool not denied: %s", tool("3"))
	}
	if !strings.Contains(history[0].Content, "skills/demo/SKILL.md") {
		t.Errorf("system prompt lacks skills index: %s", history[0].Content)
	}
	if last := events[len(events)-1]; last.Type != "message" || !strings.Contains(last.Content, "found") {
		t.Errorf("last event %+v", last)
	}
}

func TestChatSSE(t *testing.T) {
	b, err := LoadBundle(bundleDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	srv := httptest.NewServer((&Server{
		Agent:     &Agent{Bundle: b, Model: &scriptedModel{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Workspace: ws,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat", "application/json", strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events = append(events, ev)
		}
	}
	if strings.Join(events, ",") != "session,message,done" {
		t.Fatalf("events %v", events)
	}
	matches, _ := filepath.Glob(filepath.Join(ws, "sessions", "*", "*.json"))
	if len(matches) != 1 {
		t.Fatalf("session not persisted: %v", matches)
	}
}
