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
	triggerListen := flag.String("trigger-listen", "", "listen address for hook triggers from tap-hooks (empty disables)")
	triggerTokenFile := flag.String("trigger-token-file", "/run/tap/trigger/token", "bearer token tap-hooks sends to the trigger port")
	runnerURL := flag.String("runner", "http://127.0.0.1:7070", "runner base URL")
	tokenFile := flag.String("token-file", "/run/tap/token/token", "shared bearer token for the runner")
	modelURL := flag.String("model-base-url", "", "OpenAI-compatible base URL (AI gateway), ending in /v1")
	keyHeader := flag.String("model-key-header", "", "header carrying the gateway API key")
	keyFile := flag.String("model-key-file", "", "file holding the gateway API key")
	workspace := flag.String("workspace", "/workspace", "shared workspace (sessions are stored here)")
	tracesDir := flag.String("traces-dir", "", "directory each session's trace is written to after every turn, for casa-collector (empty disables traces)")
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
	hs := &harness.Server{Agent: agent, Workspace: *workspace, TracesDir: *tracesDir, Logger: logger}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           hs.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	var triggers *http.Server
	if *triggerListen != "" {
		if hs.TriggerToken, err = readTrim(*triggerTokenFile); err != nil || hs.TriggerToken == "" {
			return fmt.Errorf("--trigger-listen needs a token in %s: %v", *triggerTokenFile, err)
		}
		triggers = &http.Server{Addr: *triggerListen, Handler: hs.TriggerHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := triggers.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				logger.Error("trigger listener", "err", err)
				os.Exit(1)
			}
		}()
		logger.Info("accepting hook triggers", "addr", *triggerListen)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if triggers != nil {
			_ = triggers.Shutdown(shutdown)
		}
		_ = srv.Shutdown(shutdown)
		// Hook turns run after their request returned; give them the rest
		// of the grace period.
		hs.WaitTriggered(shutdown)
	}()
	logger.Info("listening", "addr", *listen, "agent", b.Agent.Metadata.Name, "version", b.Agent.Metadata.Version, "model", b.Agent.Harness.Model.Name, "tools", b.ToolNames(), "build", version.Get().Short())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}

func readTrim(p string) (string, error) {
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err
}
