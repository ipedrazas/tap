package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/spec"
	"github.com/ipedrazas/tap/pkg/version"
)

//go:embed agent.html
var agentHTML string

var agentPage = template.Must(template.New("agent").Funcs(template.FuncMap{
	"age":         func(t time.Time) string { return inventory.Age(t, time.Now()) },
	"short":       inventory.ShortDigest,
	"statusClass": statusClass,
	"join":        func(xs []string) string { return strings.Join(xs, ", ") },
}).Parse(agentHTML))

// toolEgress is one script tool's egress on the base branch; MCP servers'
// hosts come from their URL and are shown but not edited here.
type toolEgress struct {
	Name   string
	Egress []string
}

type agentView struct {
	Agent    *inventory.Agent
	User     string
	Build    string
	CanEdit  bool
	Team     string
	TeamNote string

	Secrets        []SecretStatus
	SecretsSync    string
	SecretsError   string
	SecretsEnabled bool

	SpecEnabled bool
	SpecError   string
	SpecSHA     string
	SpecVersion string
	SpecTitle   string
	Tools       []toolEgress
	MCP         []toolEgress

	RunsEnabled bool

	HooksEnabled bool
	HooksError   string
	HookBase     string
	Hooks        []hookView
}

func settingsRoutes(mux *http.ServeMux, d deps, list func(context.Context) ([]inventory.Agent, error)) {
	logger := d.Logger
	platform := func() *spec.Platform {
		p, err := d.Platform.get()
		if err != nil && d.Platform != nil {
			logger.Warn("platform.yaml", "err", err)
		}
		return p
	}

	mux.HandleFunc("GET /agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		a, err := find(r.Context(), list, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		p := platform()
		v := agentView{Agent: a, User: user(r), Build: version.Get().Short(), Team: a.Owner, SecretsEnabled: d.Bao != nil, SpecEnabled: d.Factory != nil}
		v.CanEdit = canEdit(p, v.User, a.Owner)
		v.RunsEnabled = d.Runs != nil
		switch {
		case p == nil:
			v.TeamNote = "Settings are read-only: the console has no platform.yaml."
		case len(p.Teams[a.Owner]) == 0:
			v.TeamNote = "Settings are read-only: platform.yaml has no team " + a.Owner + "."
		case !v.CanEdit:
			v.TeamNote = "Only members of team " + a.Owner + " can change these settings."
		}

		v.Secrets, v.SecretsSync, err = toolSecrets(r.Context(), d.Dyn, a.Namespace)
		if err != nil {
			logger.Error("read tool-secrets", "agent", a.Name, "err", err)
			v.SecretsError = "Could not read the agent's ExternalSecret."
		}
		if d.Bao != nil {
			for i := range v.Secrets {
				d.Bao.status(r.Context(), &v.Secrets[i])
			}
		}

		agentHooks(r.Context(), d, p, &v)

		if d.Factory != nil {
			s := &specs{base: d.Factory, http: &http.Client{Timeout: 20 * time.Second}}
			as, err := s.get(r.Context(), a.Name, v.User)
			if err == nil {
				var ag *spec.Agent
				if ag, _, err = spec.ParseAgent([]byte(as.AgentYAML)); err == nil {
					v.SpecSHA, v.SpecVersion, v.SpecTitle = as.SHA, ag.Metadata.Version, ag.Metadata.Title
					for _, t := range ag.Tools {
						v.Tools = append(v.Tools, toolEgress{Name: t.Name, Egress: t.Egress})
					}
					for _, m := range ag.MCP {
						host, _ := spec.MCPEgress(m.URL)
						v.MCP = append(v.MCP, toolEgress{Name: m.Name, Egress: []string{host}})
					}
				}
			}
			if err != nil {
				logger.Warn("read agent.yaml", "agent", a.Name, "err", err)
				v.SpecError = "Could not read agents/" + a.Name + "/agent.yaml from the repository: " + err.Error()
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", pageCSP)
		if err := agentPage.Execute(w, v); err != nil {
			logger.Error("render", "err", err)
		}
	})

	// editor admits a state-changing call: a signed-in member of the agent's
	// owning team, sending JSON (a cross-site form cannot without a CORS
	// preflight, which this server never allows).
	editor := func(w http.ResponseWriter, r *http.Request, body any) (*inventory.Agent, string, bool) {
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct != "application/json" {
			http.Error(w, "want application/json", http.StatusUnsupportedMediaType)
			return nil, "", false
		}
		email := user(r)
		if email == "" {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return nil, "", false
		}
		a, err := find(r.Context(), list, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return nil, "", false
		}
		if !canEdit(platform(), email, a.Owner) {
			logger.Warn("settings refused", "user", email, "agent", a.Name, "owner", a.Owner, "path", r.URL.Path)
			http.Error(w, "only members of team "+a.Owner+" can change this agent", http.StatusForbidden)
			return nil, "", false
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxSecret))
		dec.DisallowUnknownFields()
		if err := dec.Decode(body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return nil, "", false
		}
		return a, email, true
	}

	hookRoutes(mux, d, editor, platform)
	runRoutes(mux, d, list, platform)

	mux.HandleFunc("PUT /api/agents/{name}/title", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		a, email, ok := editor(w, r, &body)
		if !ok {
			return
		}
		title, err := checkTitle(body.Title)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if err := d.Titles.set(r.Context(), a.Name, title); err != nil {
			logger.Error("set title", "agent", a.Name, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		logger.Info("title set", "user", email, "agent", a.Name, "title", title)
		writeJSON(w, map[string]string{"title": title})
	})

	mux.HandleFunc("PUT /api/agents/{name}/secrets/{secret}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value string `json:"value"`
		}
		a, email, ok := editor(w, r, &body)
		if !ok {
			return
		}
		name := r.PathValue("secret")
		p := platform()
		if d.Bao == nil || p == nil {
			http.Error(w, "setting secrets is not configured on this console", http.StatusNotImplemented)
			return
		}
		if body.Value == "" || len(body.Value) > maxSecret {
			http.Error(w, "value must be 1 byte to 32 KiB", http.StatusUnprocessableEntity)
			return
		}
		declared, _, err := toolSecrets(r.Context(), d.Dyn, a.Namespace)
		if err != nil {
			http.Error(w, "cannot read the agent's ExternalSecret", http.StatusBadGateway)
			return
		}
		i := slices.IndexFunc(declared, func(s SecretStatus) bool { return s.Name == name })
		if i < 0 {
			http.Error(w, a.Name+" declares no secret "+name, http.StatusNotFound)
			return
		}
		path := declared[i].Path
		if !ownPath(p, a.Namespace, path) {
			logger.Error("secret path outside the agent's prefix", "user", email, "agent", a.Name, "secret", name, "path", path)
			http.Error(w, "secret path is outside the agent's OpenBao prefix", http.StatusForbidden)
			return
		}
		if err := d.Bao.put(r.Context(), path, body.Value); err != nil {
			logger.Error("put secret", "agent", a.Name, "secret", name, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		synced := forceSync(r.Context(), d.Dyn, a.Namespace) == nil
		// The value is never logged.
		logger.Info("secret set", "user", email, "agent", a.Name, "secret", name, "path", path, "sync", synced)
		writeJSON(w, map[string]any{"secret": name, "synced": synced})
	})

	mux.HandleFunc("POST /api/agents/{name}/pr", func(w http.ResponseWriter, r *http.Request) {
		var body EditRequest
		a, email, ok := editor(w, r, &body)
		if !ok {
			return
		}
		if d.Factory == nil {
			http.Error(w, "editing agent.yaml needs the factory (--factory-url)", http.StatusNotImplemented)
			return
		}
		if body.Title != nil {
			t, err := checkTitle(*body.Title)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			body.Title = &t
		}
		for tool, hosts := range body.Egress {
			for _, h := range hosts {
				if err := spec.CheckEgress(h); err != nil {
					http.Error(w, tool+": "+err.Error(), http.StatusUnprocessableEntity)
					return
				}
			}
		}
		if body.Title == nil && len(body.Egress) == 0 {
			http.Error(w, "nothing to change", http.StatusUnprocessableEntity)
			return
		}
		s := &specs{base: d.Factory, http: &http.Client{Timeout: 60 * time.Second}}
		pr, err := s.edit(r.Context(), a.Name, email, body)
		if err != nil {
			status := http.StatusBadGateway
			var se *statusError
			if errors.As(err, &se) && se.Status >= 400 && se.Status < 500 {
				status = se.Status
			}
			logger.Warn("edit refused", "user", email, "agent", a.Name, "err", err)
			http.Error(w, err.Error(), status)
			return
		}
		logger.Info("edit pr opened", "user", email, "agent", a.Name, "pr", pr.Number, "url", pr.URL)
		writeJSON(w, pr)
	})
}

func statusClass(s string) string {
	switch s {
	case "Ready":
		return "ready"
	case "Degraded":
		return "degraded"
	}
	return "progressing"
}
