// tap-factory-publisher is the only part of the factory that holds a GitHub
// token. It listens on loopback, next to the factory, and turns a finished
// job's files into a branch and a pull request that touches only the new
// agent's directory.
//
//	tap-factory-publisher serve  --listen 127.0.0.1:7071 --token-file ... --github-token-file ...
//	tap-factory-publisher health --listen 127.0.0.1:7071
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

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/factory"
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
	case "version", "--version":
		fmt.Println(version.Get())
	default:
		err = fmt.Errorf("unknown command %q (serve, health)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap-factory-publisher:", err)
		os.Exit(1)
	}
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7071", "listen address; keep it on loopback")
	tokenFile := fs.String("token-file", "/run/tap/publisher/token", "shared bearer token for factory calls")
	githubTokenFile := fs.String("github-token-file", "/run/tap/github/token", "GitHub token: contents and pull requests on --repo only")
	repo := fs.String("repo", "ipedrazas/tap", "repository (owner/name)")
	base := fs.String("base", "main", "branch PRs target")
	label := fs.String("label", "factory", "label added to every PR (empty for none)")
	api := fs.String("api", "https://api.github.com", "GitHub API base URL")
	proxyAddr := fs.String("egress-proxy", "", "egress proxy host:port; empty connects directly")
	egressKeyFile := fs.String("egress-key-file", "/run/tap/egress/key", "HMAC key for egress credentials")
	egressAgent := fs.String("egress-agent", "tap-factory", "name the egress proxy knows this client by")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "factory-publisher")
	token, err := readSecret(*tokenFile)
	if err != nil {
		return err
	}
	ghToken, err := readSecret(*githubTokenFile)
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second}
	if *proxyAddr != "" {
		key, err := readSecret(*egressKeyFile)
		if err != nil {
			return err
		}
		minter := egress.Minter{ProxyAddr: *proxyAddr, Agent: *egressAgent, Key: []byte(key)}
		// A fresh credential per new tunnel; the proxy checks it on CONNECT.
		transport.Proxy = func(*http.Request) (*url.URL, error) { return minter.ProxyURL("publisher", 5*time.Minute) }
	}
	p := &factory.Publisher{
		GitHub: &factory.GitHub{BaseURL: *api, Repo: *repo, Token: ghToken, HTTP: &http.Client{Transport: transport, Timeout: 60 * time.Second}},
		Base:   *base,
		Label:  *label,
		Logger: logger,
	}
	srv := &http.Server{Addr: *listen, Handler: factory.Handler(p, token), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("listening", "addr", *listen, "repo", *repo, "base", *base, "build", version.Get().Short())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func health(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7071", "publisher address")
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
