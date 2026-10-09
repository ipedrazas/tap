// tap-console is the read-only web view of the agents in the cluster. It sits
// behind the gateway's Dex login and shares pkg/inventory with tapctl.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/version"
)

//go:embed index.html
var indexHTML string

var page = template.Must(template.New("index").Funcs(template.FuncMap{
	"age":   func(t time.Time) string { return inventory.Age(t, time.Now()) },
	"short": inventory.ShortDigest,
	"join":  func(xs []string) string { return strings.Join(xs, ", ") },
	"statusClass": func(s string) string {
		switch s {
		case "Ready":
			return "ready"
		case "Degraded":
			return "degraded"
		}
		return "progressing"
	},
}).Parse(indexHTML))

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	factoryURL := flag.String("factory-url", "", "tap-factory API base URL (e.g. http://factory.tap-factory.svc:8080); empty hides the factory")
	showVersion := flag.Bool("version", false, "print the build and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get())
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "console")
	cfg, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap-console:", err)
		os.Exit(1)
	}
	cfg.Timeout = 10 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap-console:", err)
		os.Exit(1)
	}
	var factory *url.URL
	if *factoryURL != "" {
		if factory, err = url.Parse(*factoryURL); err != nil {
			fmt.Fprintln(os.Stderr, "tap-console: --factory-url:", err)
			os.Exit(1)
		}
	}
	srv := &http.Server{Addr: *listen, Handler: handler(client, logger, factory), ReadHeaderTimeout: 10 * time.Second}
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
		fmt.Fprintln(os.Stderr, "tap-console:", err)
		os.Exit(1)
	}
}

func handler(c kubernetes.Interface, logger *slog.Logger, factory *url.URL) http.Handler {
	mux := http.NewServeMux()
	if factory != nil {
		mux.HandleFunc("GET /factory", factoryPage)
		mux.Handle("/api/factory/", factoryProxy(factory))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		agents, err := inventory.List(r.Context(), c)
		if err != nil {
			logger.Error("list agents", "err", err)
			http.Error(w, "cannot list agents", http.StatusBadGateway)
			return
		}
		writeJSON(w, agents)
	})
	mux.HandleFunc("GET /api/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		a, err := inventory.Get(r.Context(), c, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, a)
	})
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, version.Get()) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		agents, err := inventory.List(r.Context(), c)
		data := map[string]any{"Agents": agents, "User": user(r), "Build": version.Get().Short(), "Error": "", "Factory": factory != nil}
		if err != nil {
			logger.Error("list agents", "err", err)
			data["Error"] = "Could not read agents from the cluster."
		}
		ready := 0
		for _, a := range agents {
			if a.Status == "Ready" {
				ready++
			}
		}
		data["Ready"] = ready
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
		if err := page.Execute(w, data); err != nil {
			logger.Error("render", "err", err)
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// user reads the email from the ID token the gateway forwards (x-tap-identity).
func user(r *http.Request) string {
	parts := strings.Split(r.Header.Get("x-tap-identity"), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.Email
}
