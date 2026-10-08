// Package runner executes declared tools. It is the only component that runs
// tool code or reads tool secrets, and it enforces agent.yaml per call:
//
//  1. unknown tools are rejected and args are validated against input_schema;
//  2. argv is built by whole-slot substitution, never through a shell;
//  3. the process gets a clean environment holding only that tool's secrets;
//  4. timeout and output cap are applied, then the process group is killed;
//  5. stdout must be JSON; stderr goes to logs, never to the model;
//  6. every call emits an audit event.
package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ipedrazas/tap/pkg/spec"
)

// Error kinds returned to the harness (and asserted by fixtures).
const (
	KindInvalidArgs    = "invalid_args"
	KindTimeout        = "timeout"
	KindExit           = "exit"
	KindBadOutput      = "bad_output"
	KindOutputTooLarge = "output_too_large"
	KindDenied         = "denied"
	KindUpstream       = "upstream"
)

const (
	DefaultTimeout   = 30 * time.Second
	DefaultOutputCap = 1 << 20
	stderrCap        = 64 << 10
	basePath         = "/usr/local/bin:/usr/bin:/bin"
)

type Request struct {
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	CallID string         `json:"call_id"`
}

type Response struct {
	CallID     string          `json:"call_id"`
	OK         bool            `json:"ok"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      *CallError      `json:"error,omitempty"`
	DurationMS int64           `json:"duration_ms"`
}

type CallError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func fail(kind, format string, args ...any) *CallError {
	return &CallError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// SecretSource returns the value of a declared secret.
type SecretSource func(name string) (string, error)

// DirSecrets reads secrets from files named after them (a mounted Secret).
func DirSecrets(dir string) SecretSource {
	return func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("secret %s: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
}

type Config struct {
	// BundleDir holds agent.yaml and tools/ (the runner/ projection).
	BundleDir string
	// Workspace is shared with the harness; tools get a HOME under it.
	Workspace string
	// TmpDir is the root for per-call temp dirs.
	TmpDir string
	// Interpreters allowed by this runner image (defense in depth for rule 1).
	Interpreters []string
	// InterpreterPaths maps an interpreter name to an absolute path, for local
	// runs where PATH holds version-manager shims. Empty in images.
	InterpreterPaths map[string]string
	Secrets          SecretSource
	// Egress returns the proxy environment for a tool that declares egress
	// (scope is the tool name). Nil means tools get no proxy at all.
	Egress func(scope string, ttl time.Duration) ([]string, error)
	// ToolUIDBase, when non-zero, runs tool i as uid/gid ToolUIDBase+i. The
	// runner must then run as root with CAP_SETUID, CAP_SETGID, CAP_CHOWN and
	// CAP_KILL, which keeps its secret files out of the tools' reach.
	ToolUIDBase int
	OutputCap   int
	Logger      *slog.Logger
}

type tool struct {
	index   int
	spec    spec.Tool
	schema  *jsonschema.Schema
	props   map[string]map[string]any
	timeout time.Duration
}

type Runner struct {
	cfg   Config
	agent *spec.Agent
	tools map[string]*tool
}

// New loads agent.yaml from cfg.BundleDir and prepares every declared tool.
func New(cfg Config) (*Runner, error) {
	data, err := os.ReadFile(filepath.Join(cfg.BundleDir, "agent.yaml"))
	if err != nil {
		return nil, err
	}
	agent, _, err := spec.ParseAgent(data)
	if err != nil {
		return nil, err
	}
	if cfg.OutputCap == 0 {
		cfg.OutputCap = DefaultOutputCap
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TmpDir == "" {
		cfg.TmpDir = os.TempDir()
	}
	defTimeout := DefaultTimeout
	if agent.Runner.Timeout != "" {
		if defTimeout, err = time.ParseDuration(agent.Runner.Timeout); err != nil {
			return nil, err
		}
	}
	r := &Runner{cfg: cfg, agent: agent, tools: map[string]*tool{}}
	for i, t := range agent.Tools {
		if len(cfg.Interpreters) > 0 && !slices.Contains(cfg.Interpreters, t.Exec[0]) {
			return nil, fmt.Errorf("tool %s: interpreter %q is not in this runner image", t.Name, t.Exec[0])
		}
		s, err := spec.CompileSchema(t.Name+".input.json", t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", t.Name, err)
		}
		var is struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(t.InputSchema, &is); err != nil {
			return nil, err
		}
		timeout := defTimeout
		if t.Timeout != "" {
			if timeout, err = time.ParseDuration(t.Timeout); err != nil {
				return nil, err
			}
		}
		r.tools[t.Name] = &tool{index: i, spec: t, schema: s, props: is.Properties, timeout: timeout}
	}
	return r, nil
}

func (r *Runner) Agent() *spec.Agent { return r.agent }

// Call runs one tool call end to end and always returns a Response.
func (r *Runner) Call(ctx context.Context, req Request) Response {
	start := time.Now()
	out, cerr, exitCode := r.call(ctx, req)
	resp := Response{CallID: req.CallID, DurationMS: time.Since(start).Milliseconds()}
	if cerr != nil {
		resp.Error = cerr
	} else {
		resp.OK, resp.Output = true, out
	}
	r.audit(req, resp, exitCode)
	return resp
}

func (r *Runner) call(ctx context.Context, req Request) (json.RawMessage, *CallError, int) {
	t, ok := r.tools[req.Tool]
	if !ok {
		if strings.Contains(req.Tool, "__") {
			return nil, fail(KindDenied, "MCP tools are not supported by this runner yet"), -1
		}
		return nil, fail(KindDenied, "unknown tool %q", req.Tool), -1
	}
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	argv, cerr := t.argv(args)
	if cerr != nil {
		return nil, cerr, -1
	}
	tmp, err := os.MkdirTemp(r.cfg.TmpDir, "call-"+safeID(req.CallID)+"-")
	if err != nil {
		return nil, fail(KindDenied, "prepare temp dir: %v", err), -1
	}
	defer os.RemoveAll(tmp)
	env, cerr := r.env(t, req.CallID, tmp)
	if cerr != nil {
		return nil, cerr, -1
	}
	return r.exec(ctx, t, argv, env, req.CallID)
}

// argv validates args and fills each {{name}} slot with exactly one value.
func (t *tool) argv(args map[string]any) ([]string, *CallError) {
	// Apply schema defaults before validation, so defaulted slots are filled.
	for name, prop := range t.props {
		if _, ok := args[name]; !ok {
			if def, ok := prop["default"]; ok {
				args[name] = def
			}
		}
	}
	if err := spec.ValidateValue(t.schema, args); err != nil {
		return nil, fail(KindInvalidArgs, "%s", oneLine(err))
	}
	argv := make([]string, len(t.spec.Exec))
	for i, slot := range t.spec.Exec {
		name, ok := templateName(slot)
		if !ok {
			argv[i] = slot
			continue
		}
		v, present := args[name]
		if !present {
			return nil, fail(KindInvalidArgs, "missing %s", name)
		}
		s, err := scalar(v)
		if err != nil {
			return nil, fail(KindInvalidArgs, "%s: %v", name, err)
		}
		// Belt and braces for rule 2: a string value can never become a flag.
		if _, isString := v.(string); isString && strings.HasPrefix(s, "-") {
			return nil, fail(KindInvalidArgs, "%s must not start with \"-\"", name)
		}
		argv[i] = s
	}
	return argv, nil
}

func templateName(slot string) (string, bool) {
	if strings.HasPrefix(slot, "{{") && strings.HasSuffix(slot, "}}") {
		return slot[2 : len(slot)-2], true
	}
	return "", false
}

func scalar(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case json.Number:
		return x.String(), nil
	case int:
		return strconv.Itoa(x), nil
	}
	return "", fmt.Errorf("value of type %T does not fit in one argv slot", v)
}

// env is the child's entire environment: no inheritance from the runner.
func (r *Runner) env(t *tool, callID, tmp string) ([]string, *CallError) {
	home := filepath.Join(r.cfg.Workspace, ".home", t.spec.Name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fail(KindDenied, "prepare home: %v", err)
	}
	if uid := r.toolUID(t); uid > 0 {
		for _, d := range []string{home, tmp} {
			if err := os.Chown(d, uid, uid); err != nil {
				return nil, fail(KindDenied, "prepare %s: %v", filepath.Base(d), err)
			}
		}
	}
	env := []string{
		"PATH=" + basePath,
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"LANG=C.UTF-8",
		"TAP_WORKSPACE=" + r.cfg.Workspace,
		"TAP_CALL_ID=" + callID,
		"NODE_NO_WARNINGS=1",
		"PYTHONDONTWRITEBYTECODE=1",
	}
	for _, name := range t.spec.Secrets {
		if r.cfg.Secrets == nil {
			return nil, fail(KindDenied, "secret %s is not available", name)
		}
		v, err := r.cfg.Secrets(name)
		if err != nil {
			// Do not leak paths or values; the log has the detail.
			r.cfg.Logger.Error("secret unavailable", "tool", t.spec.Name, "secret", name, "err", err)
			return nil, fail(KindDenied, "secret %s is not available", name)
		}
		env = append(env, name+"="+v)
	}
	if len(t.spec.Egress) > 0 && r.cfg.Egress != nil {
		proxyEnv, err := r.cfg.Egress(t.spec.Name, t.timeout+30*time.Second)
		if err != nil {
			r.cfg.Logger.Error("egress credential", "tool", t.spec.Name, "err", err)
			return nil, fail(KindDenied, "egress is not available")
		}
		env = append(env, proxyEnv...)
	}
	return env, nil
}

func (r *Runner) toolUID(t *tool) int {
	if r.cfg.ToolUIDBase == 0 {
		return 0
	}
	return r.cfg.ToolUIDBase + t.index
}

func (r *Runner) exec(ctx context.Context, t *tool, argv, env []string, callID string) (json.RawMessage, *CallError, int) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	path, ok := r.cfg.InterpreterPaths[argv[0]]
	var err error
	if !ok {
		path, err = exec.LookPath(argv[0])
	}
	if err != nil {
		return nil, fail(KindDenied, "interpreter %s not found", argv[0]), -1
	}
	cmd := &exec.Cmd{Path: path, Args: argv, Env: env, Dir: r.cfg.BundleDir}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if uid := r.toolUID(t); uid > 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: []uint32{}}
	}
	stdout := newCapped(r.cfg.OutputCap)
	stderr := newCapped(stderrCap)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return nil, fail(KindExit, "start: %v", err), -1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	var cerr *CallError
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		killGroup(cmd)
		waitErr = <-done
		cerr = fail(KindTimeout, "tool exceeded %s", t.timeout)
	case <-stdout.overflow:
		killGroup(cmd)
		waitErr = <-done
		cerr = fail(KindOutputTooLarge, "stdout exceeded %d bytes", r.cfg.OutputCap)
	}
	// Reap anything the tool left behind in its process group.
	killGroup(cmd)
	// The process may exit before select observes the overflow; Wait has
	// finished copying, so the flag is safe to read here.
	if cerr == nil && stdout.truncated {
		cerr = fail(KindOutputTooLarge, "stdout exceeded %d bytes", r.cfg.OutputCap)
	}

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if stderr.Len() > 0 {
		r.cfg.Logger.Info("tool stderr", "tool", t.spec.Name, "call_id", callID, "stderr", stderr.String(), "truncated", stderr.truncated)
	}
	if cerr != nil {
		return nil, cerr, exitCode
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			return nil, fail(KindExit, "tool exited with status %d", exitCode), exitCode
		}
		return nil, fail(KindExit, "%v", waitErr), exitCode
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if !json.Valid(out) {
		return nil, fail(KindBadOutput, "tool stdout is not a single JSON value"), exitCode
	}
	return json.RawMessage(out), nil, exitCode
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (r *Runner) audit(req Request, resp Response, exitCode int) {
	argsJSON, _ := json.Marshal(req.Args)
	sum := sha256.Sum256(argsJSON)
	attrs := []any{
		"event", "tool_call",
		"agent", r.agent.Metadata.Name,
		"tool", req.Tool,
		"call_id", req.CallID,
		"args_sha256", hex.EncodeToString(sum[:]),
		"duration_ms", resp.DurationMS,
		"ok", resp.OK,
		"exit_code", exitCode,
	}
	if resp.Error != nil {
		attrs = append(attrs, "error_kind", resp.Error.Kind)
	}
	r.cfg.Logger.Info("audit", attrs...)
}

// capped buffers up to max bytes and signals once on overflow. The buffer is
// a named field on purpose: embedding bytes.Buffer would promote ReadFrom, and
// io.Copy would use it and bypass the cap.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
	overflow  chan struct{}
	signalled bool
}

func newCapped(max int) *capped { return &capped{max: max, overflow: make(chan struct{})} }

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if len(p) > room {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		c.truncated = true
		if !c.signalled {
			c.signalled = true
			close(c.overflow)
		}
		// Report success so the child does not block; the data is discarded.
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) Len() int       { return c.buf.Len() }
func (c *capped) Bytes() []byte  { return c.buf.Bytes() }
func (c *capped) String() string { return c.buf.String() }

var _ io.Writer = (*capped)(nil)

func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "anon"
	}
	if b.Len() > 64 {
		return b.String()[:64]
	}
	return b.String()
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
