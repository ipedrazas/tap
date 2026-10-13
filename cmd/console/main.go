// tap-console is the web view of the agents in the cluster. It sits behind
// the gateway's Dex login and shares pkg/inventory with tapctl. Members of an
// agent's owning team (platform.yaml teams) may also change its settings:
// the display title (stored here), secret values (written to OpenBao), hooks
// (settings stored here, credentials in a Secret the console can only patch),
// and egress and default title (a pull request on agent.yaml, via the factory).
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

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ipedrazas/tap/pkg/hooks"
	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/version"
)

//go:embed index.html
var indexHTML string

var page = template.Must(template.New("index").Funcs(template.FuncMap{
	"age":         func(t time.Time) string { return inventory.Age(t, time.Now()) },
	"short":       inventory.ShortDigest,
	"join":        func(xs []string) string { return strings.Join(xs, ", ") },
	"statusClass": statusClass,
}).Parse(indexHTML))

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	factoryURL := flag.String("factory-url", "", "tap-factory API base URL (e.g. http://factory.tap-factory.svc:8080); empty hides the factory and spec edits")
	platformPath := flag.String("platform", "", "platform.yaml (teams, OpenBao); empty makes every agent read-only")
	namespace := flag.String("namespace", envOr("POD_NAMESPACE", "tap-system"), "namespace of the agent-settings ConfigMap")
	baoRole := flag.String("openbao-role", "tap-console", "OpenBao Kubernetes auth role for setting agent secrets")
	baoJWT := flag.String("openbao-jwt-file", "/var/run/secrets/openbao/token", "projected service account token (audience platform.yaml secrets.audience); absent disables setting secrets")
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
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tap-console:", err)
		os.Exit(1)
	}
	d := deps{K8s: client, Dyn: dyn, Logger: logger, Titles: &titles{c: client, namespace: *namespace}, Hooks: &hooks.Store{C: client, Namespace: *namespace}}
	if *factoryURL != "" {
		if d.Factory, err = url.Parse(*factoryURL); err != nil {
			fmt.Fprintln(os.Stderr, "tap-console: --factory-url:", err)
			os.Exit(1)
		}
	}
	if *platformPath != "" {
		d.Platform = &platformFile{path: *platformPath}
		p, err := d.Platform.get()
		if err != nil {
			fmt.Fprintln(os.Stderr, "tap-console: --platform:", err)
			os.Exit(1)
		}
		if _, err := os.Stat(*baoJWT); err == nil && p.Secrets.Server != "" {
			d.Bao = &openBao{cfg: p.Secrets, role: *baoRole, jwtFile: *baoJWT, http: &http.Client{Timeout: 10 * time.Second}}
		}
	}
	srv := &http.Server{Addr: *listen, Handler: handler(d), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("listening", "addr", *listen, "build", version.Get().Short(), "secrets", d.Bao != nil, "teams", d.Platform != nil)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "tap-console:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// deps is what the console talks to. Factory, Platform, Bao and Hooks are
// optional: without them the factory, editing, setting secrets and hooks are
// off (hooks also need platform.yaml hooks.publicURL).
type deps struct {
	K8s      kubernetes.Interface
	Dyn      dynamic.Interface
	Logger   *slog.Logger
	Factory  *url.URL
	Platform *platformFile
	Titles   *titles
	Bao      *openBao
	Hooks    *hooks.Store
}

const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'none'"

func handler(d deps) http.Handler {
	mux := http.NewServeMux()
	logger := d.Logger
	if d.Factory != nil {
		mux.HandleFunc("GET /factory", factoryPage)
		mux.Handle("/api/factory/", factoryProxy(d.Factory))
	}
	list := func(ctx context.Context) ([]inventory.Agent, error) {
		agents, err := inventory.List(ctx, d.K8s)
		if err == nil && d.Titles != nil {
			if terr := d.Titles.apply(ctx, agents); terr != nil {
				logger.Warn("read agent settings", "err", terr)
			}
		}
		return agents, err
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		agents, err := list(r.Context())
		if err != nil {
			logger.Error("list agents", "err", err)
			http.Error(w, "cannot list agents", http.StatusBadGateway)
			return
		}
		writeJSON(w, agents)
	})
	mux.HandleFunc("GET /api/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		a, err := find(r.Context(), list, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, a)
	})
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, version.Get()) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		agents, err := list(r.Context())
		data := map[string]any{"Agents": agents, "User": user(r), "Build": version.Get().Short(), "Error": "", "Factory": d.Factory != nil}
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
		w.Header().Set("Content-Security-Policy", pageCSP)
		if err := page.Execute(w, data); err != nil {
			logger.Error("render", "err", err)
		}
	})
	settingsRoutes(mux, d, list)
	return mux
}

func find(ctx context.Context, list func(context.Context) ([]inventory.Agent, error), name string) (*inventory.Agent, error) {
	all, err := list(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("no agent named %q is running", name)
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
