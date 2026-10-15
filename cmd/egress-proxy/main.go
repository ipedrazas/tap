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
	"github.com/ipedrazas/tap/pkg/version"
)

func main() {
	listen := flag.String("listen", ":3128", "listen address")
	namespace := flag.String("namespace", "tap-system", "namespace of the policy ConfigMaps (and the legacy keys Secret)")
	keys := flag.String("keys-secret", "egress-keys", "legacy Secret with one HMAC key per agent, read when OpenBao has none (empty: off)")
	policy := flag.String("policy-configmap", "egress-policy", "legacy ConfigMap with one <agent>.json allowlist per agent (empty: off); labelled ConfigMaps win")
	baoAddr := flag.String("openbao-addr", "", "OpenBao address for agent egress keys (empty: legacy Secret only)")
	baoAuth := flag.String("openbao-auth-mount", "kubernetes-tap", "OpenBao Kubernetes auth mount")
	baoRole := flag.String("openbao-role", "tap-egress-proxy", "OpenBao role")
	baoKV := flag.String("openbao-kv-mount", "secret", "OpenBao KV v2 mount")
	baoPrefix := flag.String("openbao-prefix", "tap-egress", "path of the keys in the KV mount (platform.yaml secrets.egressPrefix)")
	nsPrefix := flag.String("namespace-prefix", "agent-", "agent namespace prefix (platform.yaml namespacePrefix)")
	baoToken := flag.String("openbao-token-file", "/var/run/secrets/openbao/token", "projected service account token with OpenBao's audience")
	showVersion := flag.Bool("version", false, "print the build and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get())
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "egress-proxy")
	store, err := egress.NewAPIStore(*namespace, *keys, *policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap-egress-proxy:", err)
		os.Exit(1)
	}
	if *baoAddr != "" {
		store.Keys = &egress.OpenBao{
			Addr: *baoAddr, AuthMount: *baoAuth, Role: *baoRole, KVMount: *baoKV, Prefix: *baoPrefix,
			NamespacePrefix: *nsPrefix, TokenFile: *baoToken, MaxAge: 30 * time.Second,
		}
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
	logger.Info("listening", "addr", *listen, "build", version.Get().Short())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "tap-egress-proxy:", err)
		os.Exit(1)
	}
}
