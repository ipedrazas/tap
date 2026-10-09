// tap-harness runs an agent's model loop and forwards tool calls to the runner.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ipedrazas/tap/pkg/harness"
	"github.com/ipedrazas/tap/pkg/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tap-harness:", err)
		os.Exit(1)
	}
}

func run() error {
	bundle := flag.String("bundle", "/bundle", "harness projection of the bundle")
	listen := flag.String("listen", ":8080", "HTTP listen address")
	runnerURL := flag.String("runner", "http://127.0.0.1:7070", "runner base URL")
	tokenFile := flag.String("token-file", "/run/tap/token/token", "shared bearer token for the runner")
	modelURL := flag.String("model-base-url", "", "OpenAI-compatible base URL (AI gateway), ending in /v1")
	keyHeader := flag.String("model-key-header", "", "header carrying the gateway API key")
	keyFile := flag.String("model-key-file", "", "file holding the gateway API key")
	workspace := flag.String("workspace", "/workspace", "shared workspace (sessions are stored here)")
	showVersion := flag.Bool("version", false, "print the build and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get())
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "harness")
	b, err := harness.LoadBundle(*bundle)
	if err != nil {
		return err
	}
	token, err := readTrim(*tokenFile)
	if err != nil {
		return err
	}
	var key string
	if *keyFile != "" {
		if key, err = readTrim(*keyFile); err != nil {
			return err
		}
	}
	agent := &harness.Agent{
		Bundle: b,
		Model: &harness.GatewayModel{
			BaseURL:   strings.TrimRight(*modelURL, "/"),
			Route:     b.Agent.Harness.Model.Name,
			KeyHeader: *keyHeader,
			Key:       key,
			Client:    &http.Client{Timeout: 3 * time.Minute},
		},
		Runner: &harness.RunnerClient{URL: *runnerURL, Token: token, Client: &http.Client{Timeout: 6 * time.Minute}},
		Logger: logger,
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           (&harness.Server{Agent: agent, Workspace: *workspace, Logger: logger}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("listening", "addr", *listen, "agent", b.Agent.Metadata.Name, "version", b.Agent.Metadata.Version, "model", b.Agent.Harness.Model.Name, "tools", b.ToolNames(), "build", version.Get().Short())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func readTrim(p string) (string, error) {
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err
}
