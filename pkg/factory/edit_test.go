package factory

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const echoYAML = `apiVersion: tavon.ai/agent/v1
kind: Agent
metadata:
  name: echo-agent
  version: 0.1.2 # bumped by hand
  owner: platform
  description: Echoes text

# The harness.
harness:
  api: v1
  model: { provider: gateway, name: agent }

effectsPolicy:
  write: deny
  irreversible: deny

prompt: system.md

runner:
  image: registry.hiddenfield.dev/tap/runner-node@sha256:43a4b81a8490d095a1d767d49a6aaf89e862f2dfe95636c2823c8c2332a44189

tools:
  - name: echo
    description: Echo text back
    input_schema:
      type: object
      properties:
        text: { type: string }
      required: [text]
      additionalProperties: false
    exec: ["node", "tools/echo.ts", "{{text}}"]
    effects: read

  # Talks to the world.
  - name: fetch
    description: Fetch a thing
    input_schema:
      type: object
      properties:
        id: { type: string }
      additionalProperties: false
    exec: ["node", "tools/fetch.ts", "{{id}}"]
    egress:
      - api.example.com:443
      - cdn.example.com:443
    effects: read
`

func editReq(mutate func(*EditRequest)) EditRequest {
	r := EditRequest{Agent: "echo-agent", Base: base, User: "alice@hiddenfield.dev"}
	mutate(&r)
	return r
}

func ptr(s string) *string { return &s }

func TestEditAgentYAML(t *testing.T) {
	res, err := EditAgentYAML([]byte(echoYAML), editReq(func(r *EditRequest) {
		r.Egress = map[string][]string{"echo": {"api.echo.dev:443"}, "fetch": {"api.example.com:443"}}
		r.Title = ptr(`Echo "the" agent`)
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.Content)
	for _, want := range []string{
		"  version: 0.1.3 # bumped by hand\n",
		"  owner: platform\n  title: \"Echo \\\"the\\\" agent\"\n  description:",
		"# The harness.\nharness:",
		"    effects: read\n    egress: [api.echo.dev:443]\n\n  # Talks to the world.",
		"    exec: [\"node\", \"tools/fetch.ts\", \"{{id}}\"]\n    egress: [api.example.com:443]\n    effects: read\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if res.OldVersion != "0.1.2" || res.NewVersion != "0.1.3" {
		t.Errorf("versions %s -> %s", res.OldVersion, res.NewVersion)
	}
	if res.Added["echo"][0] != "api.echo.dev:443" || res.Removed["fetch"][0] != "cdn.example.com:443" || len(res.Added["fetch"]) != 0 {
		t.Errorf("added %v removed %v", res.Added, res.Removed)
	}
	if title := res.Title("echo-agent"); title != "echo-agent: update egress and title (0.1.2 -> 0.1.3)" {
		t.Errorf("title %q", title)
	}
	body := res.Body(editReq(func(*EditRequest) {}))
	for _, want := range []string{"**+ api.echo.dev:443** (widens permissions)", "− cdn.example.com:443", "rule 7", "0.1.2 → 0.1.3", "Requested by alice@hiddenfield.dev"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}

	// Removing the title and all egress, on the edited file.
	res2, err := EditAgentYAML(res.Content, editReq(func(r *EditRequest) {
		r.Egress = map[string][]string{"echo": {}}
		r.Title = ptr("")
	}))
	if err != nil {
		t.Fatal(err)
	}
	got = string(res2.Content)
	if strings.Contains(got, "title:") || strings.Contains(got, "api.echo.dev") || !strings.Contains(got, "version: 0.1.4") {
		t.Errorf("remove:\n%s", got)
	}
	if res2.Title("echo-agent") != "echo-agent: update egress and title (0.1.3 -> 0.1.4)" {
		t.Errorf("title %q", res2.Title("echo-agent"))
	}
}

func TestEditAgentYAMLRefusals(t *testing.T) {
	cases := map[string]func(*EditRequest){
		"unknown tool": func(r *EditRequest) { r.Egress = map[string][]string{"nope": {"a.com:443"}} },
		"no-op egress": func(r *EditRequest) {
			r.Egress = map[string][]string{"fetch": {"api.example.com:443", "cdn.example.com:443"}}
		},
		"no-op title": func(r *EditRequest) { r.Title = ptr("") },
		"other agent": func(r *EditRequest) { r.Agent = "other-agent"; r.Title = ptr("x") },
	}
	for name, mutate := range cases {
		if _, err := EditAgentYAML([]byte(echoYAML), editReq(mutate)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEditCheck(t *testing.T) {
	cases := map[string]func(*EditRequest){
		"nothing":        func(r *EditRequest) {},
		"wildcard":       func(r *EditRequest) { r.Egress = map[string][]string{"fetch": {"*.example.com:443"}} },
		"ip":             func(r *EditRequest) { r.Egress = map[string][]string{"fetch": {"10.0.0.1:443"}} },
		"no port":        func(r *EditRequest) { r.Egress = map[string][]string{"fetch": {"example.com"}} },
		"yaml injection": func(r *EditRequest) { r.Egress = map[string][]string{"fetch": {"a.com:443], [b.com:443"}} },
		"uppercase":      func(r *EditRequest) { r.Egress = map[string][]string{"fetch": {"API.example.com:443"}} },
		"duplicate":      func(r *EditRequest) { r.Egress = map[string][]string{"fetch": {"a.com:443", "a.com:443"}} },
		"bad agent":      func(r *EditRequest) { r.Agent = "../x"; r.Title = ptr("x") },
		"short base":     func(r *EditRequest) { r.Base = "main"; r.Title = ptr("x") },
		"no user":        func(r *EditRequest) { r.User = ""; r.Title = ptr("x") },
		"multiline":      func(r *EditRequest) { r.Title = ptr("a\nb") },
		"long title":     func(r *EditRequest) { r.Title = ptr(strings.Repeat("x", 81)) },
	}
	for name, mutate := range cases {
		r := editReq(mutate)
		if err := r.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every agent in the repo can have its egress and title edited without
// touching anything else (the data check in EditAgentYAML would refuse).
func TestEditRepoAgents(t *testing.T) {
	paths, _ := filepath.Glob("../../testdata/agents/*/agent.yaml")
	if len(paths) == 0 {
		t.Skip("no agents")
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(filepath.Dir(p))
		var doc struct {
			Tools []struct {
				Name   string   `json:"name"`
				Egress []string `json:"egress"`
			} `json:"tools"`
		}
		data, _ := asData(b)
		raw, _ := json.Marshal(data)
		_ = json.Unmarshal(raw, &doc)
		egress := map[string][]string{}
		for _, tool := range doc.Tools {
			egress[tool.Name] = append(append([]string{}, tool.Egress...), "added.example.com:443")
		}
		if _, err := EditAgentYAML(b, EditRequest{Agent: name, Base: base, User: "u", Title: ptr("New title"), Egress: egress}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEditHandler(t *testing.T) {
	f := &fakeGitHub{files: map[string]string{
		"agents/echo-agent/agent.yaml@" + base: echoYAML,
	}, blobs: map[string]string{}}
	gh := httptest.NewServer(f.handler(t))
	t.Cleanup(gh.Close)
	p := &Publisher{
		GitHub: &GitHub{BaseURL: gh.URL, Repo: "o/r", Token: "tok"},
		Base:   "main", Label: "factory",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:    func() time.Time { return time.Unix(1700000000, 0) },
	}
	h := Handler(p, "secret")

	w := call(t, h, "GET", "/v1/agents/echo-agent", "secret", nil)
	var got struct{ SHA, AgentYaml string }
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got.SHA != base || got.AgentYaml != echoYAML {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "GET", "/v1/agents/nope-agent", "secret", nil); w.Code != 404 {
		t.Errorf("missing agent: %d", w.Code)
	}
	if w := call(t, h, "GET", "/v1/agents/echo-agent", "", nil); w.Code != 401 {
		t.Errorf("no token: %d", w.Code)
	}

	req := editReq(func(r *EditRequest) { r.Egress = map[string][]string{"echo": {"api.echo.dev:443"}} })
	w = call(t, h, "POST", "/v1/edit", "secret", req)
	if w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	if f.ref != "refs/heads/console/echo-agent-1700000000" || f.parent != base || f.treeBase != "tree-012345" {
		t.Errorf("ref %s parent %s tree %s", f.ref, f.parent, f.treeBase)
	}
	if len(f.tree) != 1 || f.tree[0].Path != "agents/echo-agent/agent.yaml" || f.tree[0].Mode != "100644" {
		t.Errorf("tree %+v", f.tree)
	}
	content, _ := base64.StdEncoding.DecodeString(f.blobs[f.tree[0].SHA])
	if !strings.Contains(string(content), "egress: [api.echo.dev:443]") {
		t.Errorf("blob:\n%s", content)
	}
	if f.pr["head"] != "console/echo-agent-1700000000" || f.pr["base"] != "main" || f.pr["title"] != "echo-agent: update egress (0.1.2 -> 0.1.3)" {
		t.Errorf("pr %+v", f.pr)
	}
	if len(f.labels) != 1 || f.labels[0] != "console" {
		t.Errorf("labels %v", f.labels)
	}

	noop := editReq(func(r *EditRequest) { r.Egress = map[string][]string{"echo": {}} })
	if w := call(t, h, "POST", "/v1/edit", "secret", noop); w.Code != 422 {
		t.Errorf("no-op: %d %s", w.Code, w.Body)
	}
	other := editReq(func(r *EditRequest) { r.Agent = "nope-agent"; r.Title = ptr("x") })
	if w := call(t, h, "POST", "/v1/edit", "secret", other); w.Code != 404 {
		t.Errorf("missing: %d %s", w.Code, w.Body)
	}
	unknown := map[string]any{"agent": "echo-agent", "base": base, "user": "u", "title": "x", "files": []string{}}
	if w := call(t, h, "POST", "/v1/edit", "secret", unknown); w.Code != 400 {
		t.Errorf("unknown field: %d", w.Code)
	}
	if w := call(t, h, "POST", "/v1/edit", "", req); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
}
