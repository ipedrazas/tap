package factory

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const base = "0123456789abcdef0123456789abcdef01234567"

// fakeGitHub records the Git Data API calls the publisher makes.
type fakeGitHub struct {
	mu       sync.Mutex
	existing map[string]bool // "path@ref"
	blobs    map[string]string
	tree     []treeEntry
	treeBase string
	parent   string
	ref      string
	pr       map[string]any
	labels   []string
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	decode := func(r *http.Request, v any) {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			t.Errorf("%s %s: %v", r.Method, r.URL, err)
		}
	}
	mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"object":{"sha":"`+base+`"}}`)
	})
	mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if f.existing[r.PathValue("path")+"@"+r.URL.Query().Get("ref")] {
			io.WriteString(w, `[]`)
			return
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"message":"Not Found"}`)
	})
	mux.HandleFunc("GET /repos/o/r/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"tree":{"sha":"tree-`+r.PathValue("sha")[:6]+`"}}`)
	})
	mux.HandleFunc("POST /repos/o/r/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		decode(r, &in)
		f.mu.Lock()
		sha := "blob" + string(rune('a'+len(f.blobs)))
		f.blobs[sha] = in["content"]
		f.mu.Unlock()
		io.WriteString(w, `{"sha":"`+sha+`"}`)
	})
	mux.HandleFunc("POST /repos/o/r/git/trees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BaseTree string      `json:"base_tree"`
			Tree     []treeEntry `json:"tree"`
		}
		decode(r, &in)
		f.treeBase, f.tree = in.BaseTree, in.Tree
		io.WriteString(w, `{"sha":"newtree"}`)
	})
	mux.HandleFunc("POST /repos/o/r/git/commits", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
		}
		decode(r, &in)
		if in.Tree != "newtree" || len(in.Parents) != 1 {
			t.Errorf("commit: %+v", in)
		}
		f.parent = in.Parents[0]
		io.WriteString(w, `{"sha":"newcommit"}`)
	})
	mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		decode(r, &in)
		f.ref = in["ref"]
		w.WriteHeader(201)
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		decode(r, &f.pr)
		w.WriteHeader(201)
		io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42"}`)
	})
	mux.HandleFunc("POST /repos/o/r/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Labels []string }
		decode(r, &in)
		f.labels = in.Labels
		io.WriteString(w, `[]`)
	})
	return mux
}

func newTestPublisher(t *testing.T) (*fakeGitHub, http.Handler) {
	f := &fakeGitHub{existing: map[string]bool{"agents/echo-agent@main": true}, blobs: map[string]string{}}
	gh := httptest.NewServer(f.handler(t))
	t.Cleanup(gh.Close)
	p := &Publisher{
		GitHub: &GitHub{BaseURL: gh.URL, Repo: "o/r", Token: "tok"},
		Base:   "main",
		Label:  "factory",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return f, Handler(p, "secret")
}

func validRequest() PRRequest {
	return PRRequest{
		Job: "j1", Agent: "new-agent", Base: base, Title: "Add new-agent", Body: "report",
		Files: []File{
			{Path: "agents/new-agent/agent.yaml", Content: []byte("apiVersion: tap/v1\n")},
			{Path: "agents/new-agent/tools/run.sh", Content: []byte("#!/bin/sh\n"), Executable: true},
		},
	}
}

func call(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPublish(t *testing.T) {
	f, h := newTestPublisher(t)
	w := call(t, h, "POST", "/v1/pr", "secret", validRequest())
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var pr PullRequest
	_ = json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.Number != 42 || pr.URL == "" {
		t.Fatalf("pr: %+v", pr)
	}
	if f.treeBase != "tree-012345" || f.parent != base {
		t.Errorf("not built on base: tree %s parent %s", f.treeBase, f.parent)
	}
	if f.ref != "refs/heads/factory/new-agent-j1" {
		t.Errorf("ref %s", f.ref)
	}
	if len(f.tree) != 2 || f.tree[1].Mode != "100755" || f.tree[0].Mode != "100644" {
		t.Errorf("tree %+v", f.tree)
	}
	if f.pr["head"] != "factory/new-agent-j1" || f.pr["base"] != "main" || f.pr["body"] != "report" {
		t.Errorf("pr request %+v", f.pr)
	}
	if len(f.labels) != 1 || f.labels[0] != "factory" {
		t.Errorf("labels %v", f.labels)
	}
}

func TestHead(t *testing.T) {
	_, h := newTestPublisher(t)
	w := call(t, h, "GET", "/v1/head", "secret", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), base) {
		t.Fatalf("head: %d %s", w.Code, w.Body)
	}
}

func TestPublishRefusals(t *testing.T) {
	_, h := newTestPublisher(t)
	if w := call(t, h, "POST", "/v1/pr", "", validRequest()); w.Code != 401 {
		t.Errorf("no token: %d", w.Code)
	}
	if w := call(t, h, "POST", "/v1/pr", "wrong", validRequest()); w.Code != 401 {
		t.Errorf("wrong token: %d", w.Code)
	}
	if w := call(t, h, "GET", "/v1/head", "", nil); w.Code != 401 {
		t.Errorf("head without token: %d", w.Code)
	}
	exists := validRequest()
	exists.Agent = "echo-agent"
	for i := range exists.Files {
		exists.Files[i].Path = strings.Replace(exists.Files[i].Path, "new-agent", "echo-agent", 1)
	}
	if w := call(t, h, "POST", "/v1/pr", "secret", exists); w.Code != 409 {
		t.Errorf("existing agent: %d %s", w.Code, w.Body)
	}
}

func TestCheck(t *testing.T) {
	cases := map[string]func(r *PRRequest){
		"outside agent dir":  func(r *PRRequest) { r.Files[1].Path = "agents/other/x" },
		"workflow":           func(r *PRRequest) { r.Files[1].Path = ".github/workflows/ci.yml" },
		"traversal":          func(r *PRRequest) { r.Files[1].Path = "agents/new-agent/../../Taskfile.yml" },
		"dot segment":        func(r *PRRequest) { r.Files[1].Path = "agents/new-agent/./x" },
		"double slash":       func(r *PRRequest) { r.Files[1].Path = "agents/new-agent//x" },
		"git dir":            func(r *PRRequest) { r.Files[1].Path = "agents/new-agent/.git/config" },
		"absolute":           func(r *PRRequest) { r.Files[1].Path = "/agents/new-agent/x" },
		"backslash":          func(r *PRRequest) { r.Files[1].Path = `agents/new-agent/a\b` },
		"dir itself":         func(r *PRRequest) { r.Files[1].Path = "agents/new-agent/" },
		"duplicate":          func(r *PRRequest) { r.Files[1].Path = r.Files[0].Path },
		"no agent.yaml":      func(r *PRRequest) { r.Files = r.Files[1:] },
		"bad agent name":     func(r *PRRequest) { r.Agent = "Bad_Name" },
		"bad job":            func(r *PRRequest) { r.Job = "../x" },
		"short base":         func(r *PRRequest) { r.Base = "main" },
		"multiline title":    func(r *PRRequest) { r.Title = "a\nb" },
		"empty title":        func(r *PRRequest) { r.Title = "" },
		"too large":          func(r *PRRequest) { r.Files[1].Content = make([]byte, MaxTotalSize+1) },
		"no files":           func(r *PRRequest) { r.Files = nil },
		"agent name in path": func(r *PRRequest) { r.Files[1].Path = "agents/new-agentx/x" },
	}
	if r := validRequest(); r.Check() != nil {
		t.Fatalf("valid request refused: %v", r.Check())
	}
	for name, mutate := range cases {
		r := validRequest()
		mutate(&r)
		if err := r.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
