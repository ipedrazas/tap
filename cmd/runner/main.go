// tap-runner executes an agent's declared tools for the harness.
//
//	tap-runner serve   --bundle /bundle --listen 127.0.0.1:7070 --token-file ... --secrets-dir ...
//	tap-runner health  --listen 127.0.0.1:7070
//	tap-runner test    --bundle /bundle --tests /tests
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// Root CAs for the runner's own TLS (MCP) when the base image ships none
	// (node:*-slim has no ca-certificates; Node itself bundles its roots).
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/mcp"
	"github.com/ipedrazas/tap/pkg/runner"
	"github.com/ipedrazas/tap/pkg/spec"
	"github.com/ipedrazas/tap/pkg/version"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "health":
		err = health(args)
	case "test":
		err = test(args)
	case "version", "--version":
		fmt.Println(version.Get())
	default:
		err = fmt.Errorf("unknown command %q (serve, health, test)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap-runner:", err)
		os.Exit(1)
	}
}

type common struct {
	bundle, workspace, tmp, interpreters string
	interpPaths                          multiFlag
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func commonFlags(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.bundle, "bundle", "/bundle", "runner projection of the bundle (agent.yaml, tools/)")
	fs.StringVar(&c.workspace, "workspace", "/workspace", "shared workspace")
	fs.StringVar(&c.tmp, "tmp", os.TempDir(), "root for per-call temp dirs")
	fs.StringVar(&c.interpreters, "interpreters-file", "/etc/tap/interpreters", "interpreters this image provides, one per line (empty path disables the check)")
	fs.Var(&c.interpPaths, "interpreter-path", "name=/abs/path for local runs (repeatable)")
	return c
}

func (c *common) config(logger *slog.Logger, secrets runner.SecretSource) (runner.Config, error) {
	cfg := runner.Config{BundleDir: c.bundle, Workspace: c.workspace, TmpDir: c.tmp, Secrets: secrets, Logger: logger, InterpreterPaths: map[string]string{}}
	for _, kv := range c.interpPaths {
		name, path, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(path, "/") {
			return cfg, fmt.Errorf("--interpreter-path %q: want name=/abs/path", kv)
		}
		cfg.InterpreterPaths[name] = path
	}
	if c.interpreters != "" {
		data, err := os.ReadFile(c.interpreters)
		if err != nil {
			return cfg, err
		}
		cfg.Interpreters = strings.Fields(string(data))
	}
	return cfg, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c := commonFlags(fs)
	listen := fs.String("listen", "127.0.0.1:7070", "listen address; keep it on loopback")
	tokenFile := fs.String("token-file", "/run/tap/token/token", "shared bearer token for harness calls")
	secretsDir := fs.String("secrets-dir", "/run/tap/secrets", "mounted tool secrets, one file per secret")
	concurrency := fs.Int("concurrency", 4, "maximum concurrent tool calls")
	proxyAddr := fs.String("egress-proxy", "", "egress proxy host:port; empty disables egress")
	egressKeyFile := fs.String("egress-key-file", "/run/tap/egress/key", "HMAC key for per-call egress credentials")
	toolUIDBase := fs.Int("tool-uid-base", 0, "run tool i as uid/gid base+i (runner must be root with SETUID/SETGID/CHOWN/KILL)")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "runner")
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return err
	}
	cfg, err := c.config(logger, runner.DirSecrets(*secretsDir))
	if err != nil {
		return err
	}
	cfg.ToolUIDBase = *toolUIDBase
	if *toolUIDBase == 0 && os.Getuid() == 0 {
		return fmt.Errorf("refusing to run tools as root; set --tool-uid-base")
	}
	if *proxyAddr != "" {
		key, err := os.ReadFile(*egressKeyFile)
		if err != nil {
			return err
		}
		minter := egress.Minter{ProxyAddr: *proxyAddr, Agent: os.Getenv("TAP_AGENT"), Key: []byte(strings.TrimSpace(string(key)))}
		cfg.Egress = minter.Env
		cfg.MCP = func(s spec.MCPServer, header http.Header) (runner.MCPBackend, error) {
			scope := "mcp:" + s.Name
			transport := &http.Transport{
				// A fresh credential per new tunnel; the proxy checks it on CONNECT.
				Proxy:               func(*http.Request) (*url.URL, error) { return minter.ProxyURL(scope, 5*time.Minute) },
				TLSHandshakeTimeout: 10 * time.Second,
				IdleConnTimeout:     90 * time.Second,
			}
			return &mcp.Client{URL: s.URL, Header: header, HTTP: &http.Client{Transport: transport}}, nil
		}
	}
	r, err := runner.New(cfg)
	if err != nil {
		return err
	}
	if minterAgent := os.Getenv("TAP_AGENT"); *proxyAddr != "" && minterAgent != r.Agent().Metadata.Name {
		return fmt.Errorf("TAP_AGENT %q does not match bundle %q", minterAgent, r.Agent().Metadata.Name)
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           runner.Handler(r, strings.TrimSpace(string(token)), *concurrency),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("listening", "addr", *listen, "agent", r.Agent().Metadata.Name, "tools", len(r.Agent().Tools), "mcp_servers", len(r.Agent().MCP), "build", version.Get().Short())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func health(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7070", "runner address")
	_ = fs.Parse(args)
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + *listen + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

func test(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	c := commonFlags(fs)
	tests := fs.String("tests", "/tests", "fixtures directory")
	_ = fs.Parse(args)

	// Tool stderr and audit events go to stderr so stdout is the report.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "runner")
	if err := os.MkdirAll(c.workspace, 0o755); err != nil {
		return err
	}
	cfg, err := c.config(logger, nil)
	if err != nil {
		return err
	}
	r, err := runner.New(cfg)
	if err != nil {
		return err
	}
	fixtures, err := spec.LoadFixtures(*tests)
	if err != nil {
		return err
	}
	mock, err := egress.StartMock(c.tmp)
	if err != nil {
		return err
	}
	defer mock.Close()
	results := runner.RunFixtures(context.Background(), r, fixtures, mock)
	var passed, failed, skipped int
	for _, res := range results {
		switch {
		case res.Skipped != "":
			skipped++
			fmt.Printf("SKIP %s / %s: %s\n", res.Tool, res.Case, res.Skipped)
		case res.Failure != "":
			failed++
			fmt.Printf("FAIL %s / %s (%s): %s\n", res.Tool, res.Case, res.File, res.Failure)
		default:
			passed++
			fmt.Printf("PASS %s / %s (%dms)\n", res.Tool, res.Case, res.Resp.DurationMS)
		}
	}
	fmt.Printf("\n%d passed, %d failed, %d skipped\n", passed, failed, skipped)
	if failed > 0 || passed == 0 && len(results) > 0 {
		return fmt.Errorf("fixtures failed")
	}
	return nil
}
