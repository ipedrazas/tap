// tap-harness runs an agent's model loop and forwards tool calls to the runner.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/harness"
	"github.com/ipedrazas/tap/pkg/s3"
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
	sessionsEndpoint := flag.String("sessions-endpoint", "", "S3 endpoint to upload session traces to (empty disables export)")
	sessionsBucket := flag.String("sessions-bucket", "", "bucket for session traces")
	sessionsRegion := flag.String("sessions-region", "auto", "region for session trace signing")
	sessionsCreds := flag.String("sessions-credentials-dir", "/run/tap/sessions", "directory holding access-key-id and secret-access-key")
	egressProxy := flag.String("egress-proxy", "", "egress proxy host:port for session uploads")
	egressKeyFile := flag.String("egress-key-file", "/run/tap/egress/key", "agent egress key, to mint proxy credentials")
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
	hs := &harness.Server{Agent: agent, Workspace: *workspace, Logger: logger}
	if *sessionsEndpoint != "" {
		store, err := sessionStore(b.Agent.Metadata.Name, *sessionsEndpoint, *sessionsBucket, *sessionsRegion, *sessionsCreds, *egressProxy, *egressKeyFile)
		if err != nil {
			return err
		}
		hs.Exporter = harness.NewExporter(store, logger)
		logger.Info("exporting session traces", "endpoint", *sessionsEndpoint, "bucket", *sessionsBucket)
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           hs.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		// Handlers have returned, so nothing enqueues any more.
		if hs.Exporter != nil {
			hs.Exporter.Close(shutdown)
		}
	}()
	logger.Info("listening", "addr", *listen, "agent", b.Agent.Metadata.Name, "version", b.Agent.Metadata.Version, "model", b.Agent.Harness.Model.Name, "tools", b.ToolNames(), "build", version.Get().Short())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}

// sessionStore is the bucket session traces go to. With a proxy, uploads
// leave through the egress proxy under the harness:sessions scope, with a
// fresh credential per connection.
func sessionStore(agentName, endpoint, bucket, region, credsDir, proxy, keyFile string) (*s3.Client, error) {
	if bucket == "" {
		return nil, fmt.Errorf("--sessions-bucket is required with --sessions-endpoint")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		key, err := readTrim(keyFile)
		if err != nil {
			return nil, err
		}
		m := egress.Minter{ProxyAddr: proxy, Agent: agentName, Key: []byte(key)}
		transport.Proxy = func(*http.Request) (*url.URL, error) { return m.ProxyURL(egress.HarnessSessionsScope, 5*time.Minute) }
	}
	return &s3.Client{
		Endpoint: endpoint,
		Region:   region,
		Bucket:   bucket,
		Credentials: func() (s3.Credentials, error) {
			id, err := readTrim(filepath.Join(credsDir, "access-key-id"))
			if err != nil {
				return s3.Credentials{}, err
			}
			secret, err := readTrim(filepath.Join(credsDir, "secret-access-key"))
			return s3.Credentials{AccessKeyID: id, SecretAccessKey: secret}, err
		},
		HTTP: &http.Client{Timeout: time.Minute, Transport: transport},
	}, nil
}

func readTrim(p string) (string, error) {
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err
}
