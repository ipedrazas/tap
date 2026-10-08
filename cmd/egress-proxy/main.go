// tap-egress-proxy is the only way out of the cluster for tool code: an HTTP
// CONNECT proxy that allows each call only the hosts its tool declares.
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
	"syscall"
	"time"

	"github.com/ipedrazas/tap/pkg/egress"
)

func main() {
	listen := flag.String("listen", ":3128", "listen address")
	namespace := flag.String("namespace", "tap-system", "namespace of the keys Secret and policy ConfigMap")
	keys := flag.String("keys-secret", "egress-keys", "Secret with one HMAC key per agent")
	policy := flag.String("policy-configmap", "egress-policy", "ConfigMap with one <agent>.json allowlist per agent")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "egress-proxy")
	store, err := egress.NewAPIStore(*namespace, *keys, *policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap-egress-proxy:", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           &egress.Proxy{Store: store, Logger: logger},
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("listening", "addr", *listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "tap-egress-proxy:", err)
		os.Exit(1)
	}
}
