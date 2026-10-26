// tap-hooks receives hook deliveries from outside the cluster (GitHub, CI,
// Tigris object notifications) at POST /h/<agent>/<hook>, verifies them
// against the hook's credential and asks the agent's harness to run a turn.
// It is the only tap service exposed to the internet, and it can do nothing
// but that: read hook settings and call agents' trigger ports.
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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ipedrazas/tap/pkg/hooks"
	"github.com/ipedrazas/tap/pkg/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tap-hooks:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", ":8080", "listen address")
	namespace := flag.String("namespace", envOr("POD_NAMESPACE", "tap-system"), "namespace of ConfigMap agent-settings and Secret agent-hooks")
	agentURL := flag.String("agent-url", "http://agent.agent-{agent}.svc.cluster.local:8081", "an agent's trigger port; {agent} is replaced by its name")
	tokenFile := flag.String("trigger-token-file", "/run/tap/trigger/token", "bearer token for agents' trigger ports")
	perMinute := flag.Int("per-minute", 30, "accepted deliveries per hook per minute")
	ttl := flag.Duration("cache-ttl", 10*time.Second, "how long hook settings are cached")
	showVersion := flag.Bool("version", false, "print the build and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get())
		return nil
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "hooks")
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	cfg.Timeout = 10 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil || len(strings.TrimSpace(string(token))) == 0 {
		return fmt.Errorf("trigger token %s: %v", *tokenFile, err)
	}
	cache := &hooks.Cache{Store: &hooks.Store{C: client, Namespace: *namespace}, TTL: *ttl}
	trigger := &hooks.HarnessTrigger{URL: *agentURL, Token: strings.TrimSpace(string(token)), Client: &http.Client{Timeout: 15 * time.Second}}
	rc := &hooks.Receiver{Lookup: cache.Lookup, Trigger: trigger.Trigger, Logger: logger, PerMinute: *perMinute}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           rc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		if n := rc.Pending(); n > 0 {
			// Held object-storage runs live in memory; their senders got a 202.
			logger.Warn("shutting down with scheduled runs; they are dropped", "pending", n)
		}
	}()
	logger.Info("listening", "addr", *listen, "agent_url", *agentURL, "build", version.Get().Short())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
