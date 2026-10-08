package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The unit tests use sh as the interpreter: runner.New does not apply rule 1
// (tapctl does), which keeps these tests free of node/python.
const agentYAML = `apiVersion: tavon.ai/agent/v1
kind: Agent
metadata: { name: unit, version: 0.0.1, owner: me, description: unit test agent }
harness: { api: v1, model: { provider: gateway, name: sim } }
effectsPolicy: { write: deny, irreversible: deny }
prompt: system.md
runner:
  image: registry.hiddenfield.dev/tap/runner-node@sha256:1111111111111111111111111111111111111111111111111111111111111111
  timeout: 2s
tools:
  - name: echo_args
    description: print argv as JSON
    input_schema:
      type: object
      properties:
        word: { type: string, pattern: "^[a-z -;$]+$" }
        count: { type: integer, default: 3 }
      required: [word]
      additionalProperties: false
    exec: ["sh", "tools/echo_args.sh", "{{word}}", "{{count}}"]
    effects: read
  - name: env_dump
    description: print selected env as JSON
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["sh", "tools/env_dump.sh"]
    secrets: [TOKEN]
    effects: read
  - name: no_secrets
    description: print selected env as JSON
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["sh", "tools/env_dump.sh"]
    effects: read
  - name: sleeper
    description: background child then hang
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["sh", "tools/sleeper.sh"]
    timeout: 300ms
    effects: read
  - name: flood
    description: write too much
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["sh", "tools/flood.sh"]
    effects: read
  - name: not_json
    description: write text
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["sh", "tools/not_json.sh"]
    effects: read
  - name: fails
    description: exit 3 with a secret-looking stderr
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["sh", "tools/fails.sh"]
    effects: read
secrets:
  TOKEN: { from: vault://unit/token }
`

var scripts = map[string]string{
	"echo_args.sh": `printf '{"word":"%s","count":"%s","argc":%d}' "$1" "$2" "$#"`,
	"env_dump.sh":  `printf '{"token":"%s","runner_secret":"%s","home":"%s","path":"%s"}' "$TOKEN" "$RUNNER_SECRET" "$HOME" "$PATH"`,
	"sleeper.sh":   "sleep 30 &\necho $! > \"$TAP_WORKSPACE/child.pid\"\nsleep 30\n",
	"flood.sh":     `yes '"xxxxxxxxxxxxxxxxxxxx"' | head -c 3000000`,
	"not_json.sh":  `echo hello`,
	"fails.sh":     "echo 'stack trace with sk-secret' >&2\nexit 3\n",
}

func newRunner(t *testing.T) *Runner {
	t.Helper()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(agentYAML), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "tools"), 0o755))
	for name, body := range scripts {
		must(t, os.WriteFile(filepath.Join(dir, "tools", name), []byte(body), 0o644))
	}
	t.Setenv("RUNNER_SECRET", "must-not-leak")
	r, err := New(Config{
		BundleDir: dir,
		Workspace: t.TempDir(),
		TmpDir:    t.TempDir(),
		Secrets: func(name string) (string, error) {
			if name == "TOKEN" {
				return "tok-123", nil
			}
			return "", fmt.Errorf("no %s", name)
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, r *Runner, tool string, args map[string]any) (Response, map[string]any) {
	t.Helper()
	resp := r.Call(context.Background(), Request{Tool: tool, Args: args, CallID: "c-1"})
	var out map[string]any
	if resp.OK {
		must(t, json.Unmarshal(resp.Output, &out))
	}
	return resp, out
}

func TestSubstitution(t *testing.T) {
	r := newRunner(t)
	// Shell metacharacters stay one literal argv element; no shell is involved.
	resp, out := call(t, r, "echo_args", map[string]any{"word": "a b; $x"})
	if !resp.OK {
		t.Fatalf("%+v", resp.Error)
	}
	if out["word"] != "a b; $x" || out["count"] != "3" || out["argc"] != float64(2) {
		t.Fatalf("got %v", out)
	}
}

func TestInvalidArgs(t *testing.T) {
	r := newRunner(t)
	for name, args := range map[string]map[string]any{
		"missing required": {},
		"extra property":   {"word": "ok", "evil": "x"},
		"pattern":          {"word": "UPPER"},
		"wrong type":       {"word": "ok", "count": "3"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, _ := call(t, r, "echo_args", args)
			if resp.OK || resp.Error.Kind != KindInvalidArgs {
				t.Fatalf("got %+v", resp)
			}
		})
	}
}

func TestLeadingDashRejected(t *testing.T) {
	r := newRunner(t)
	// The schema pattern allows "-" here; the runner still refuses a flag-like value.
	resp, _ := call(t, r, "echo_args", map[string]any{"word": "-rf"})
	if resp.OK || resp.Error.Kind != KindInvalidArgs {
		t.Fatalf("got %+v", resp)
	}
}

func TestUnknownTool(t *testing.T) {
	r := newRunner(t)
	resp, _ := call(t, r, "rm_rf", nil)
	if resp.OK || resp.Error.Kind != KindDenied {
		t.Fatalf("got %+v", resp)
	}
}

func TestCleanEnvironment(t *testing.T) {
	r := newRunner(t)
	_, out := call(t, r, "env_dump", nil)
	if out["token"] != "tok-123" {
		t.Fatalf("declared secret missing: %v", out)
	}
	if out["runner_secret"] != "" {
		t.Fatalf("runner environment leaked into tool: %v", out)
	}
	if out["path"] != basePath {
		t.Fatalf("PATH = %v", out["path"])
	}
	_, out = call(t, r, "no_secrets", nil)
	if out["token"] != "" {
		t.Fatalf("secret leaked to a tool that did not declare it: %v", out)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	r := newRunner(t)
	start := time.Now()
	resp, _ := call(t, r, "sleeper", nil)
	if resp.OK || resp.Error.Kind != KindTimeout {
		t.Fatalf("got %+v", resp)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	pid, err := os.ReadFile(filepath.Join(r.cfg.Workspace, "child.pid"))
	must(t, err)
	// kill -0 on the background child must fail: the whole group is gone.
	time.Sleep(100 * time.Millisecond)
	if err := exec0(strings.TrimSpace(string(pid))); err == nil {
		t.Fatalf("background child %s survived", pid)
	}
}

func TestOutputCap(t *testing.T) {
	r := newRunner(t)
	resp, _ := call(t, r, "flood", nil)
	if resp.OK || resp.Error.Kind != KindOutputTooLarge {
		t.Fatalf("got %+v %+v", resp, resp.Error)
	}
}

func TestBadOutputAndExit(t *testing.T) {
	r := newRunner(t)
	if resp, _ := call(t, r, "not_json", nil); resp.OK || resp.Error.Kind != KindBadOutput {
		t.Fatalf("got %+v", resp)
	}
	resp, _ := call(t, r, "fails", nil)
	if resp.OK || resp.Error.Kind != KindExit {
		t.Fatalf("got %+v", resp)
	}
	if strings.Contains(resp.Error.Message, "sk-secret") {
		t.Fatal("stderr reached the response")
	}
}

func TestHandlerAuth(t *testing.T) {
	srv := httptest.NewServer(Handler(newRunner(t), "s3cret", 2))
	defer srv.Close()
	body := `{"tool":"echo_args","args":{"word":"hi"},"call_id":"c-9"}`
	for token, want := range map[string]int{"": 401, "wrong": 401, "s3cret": 200} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/call", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("token %q: got %d want %d", token, resp.StatusCode, want)
		}
	}
}

func exec0(pid string) error {
	var n int
	if _, err := fmt.Sscanf(pid, "%d", &n); err != nil {
		return err
	}
	return syscall.Kill(n, 0)
}

func TestCappedHasNoReadFrom(t *testing.T) {
	// io.Copy would use ReadFrom and skip the cap entirely.
	if _, ok := any(newCapped(1)).(io.ReaderFrom); ok {
		t.Fatal("capped must not implement io.ReaderFrom")
	}
}
