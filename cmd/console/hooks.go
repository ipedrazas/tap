package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ipedrazas/tap/pkg/hooks"
	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/spec"
)

// hookView is one hook on the settings page. Credentials are never read back:
// the console can only patch Secret agent-hooks.
type hookView struct {
	hooks.Hook
	URL string
}

// EventsText is the event filter, one per line, for the textarea.
func (h hookView) EventsText() string { return strings.Join(h.Events, "\n") }

// agentHooks fills the page's hooks section.
func agentHooks(ctx context.Context, d deps, p *spec.Platform, v *agentView) {
	if d.Hooks == nil || p == nil || !p.Hooks.Enabled() {
		return
	}
	v.HooksEnabled = true
	v.HookBase = p.Hooks.URL(v.Agent.Name, "")
	list, err := d.Hooks.List(ctx, v.Agent.Name)
	if err != nil {
		d.Logger.Error("list hooks", "agent", v.Agent.Name, "err", err)
		v.HooksError = "Could not read this agent's hooks."
		return
	}
	for _, h := range list {
		v.Hooks = append(v.Hooks, hookView{Hook: h, URL: p.Hooks.URL(h.Agent, h.Name)})
	}
}

type hookBody struct {
	Name          string   `json:"name"`
	Auth          string   `json:"auth"`
	Enabled       bool     `json:"enabled"`
	Events        []string `json:"events"`
	Prompt        string   `json:"prompt"`
	SettleSeconds int      `json:"settleSeconds"`
}

// hookCreated is shown once: the credential cannot be read back.
type hookCreated struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Auth   string `json:"auth"`
	Secret string `json:"secret"`
}

type editorFunc func(w http.ResponseWriter, r *http.Request, body any) (*inventory.Agent, string, bool)

func hookRoutes(mux *http.ServeMux, d deps, editor editorFunc, platform func() *spec.Platform) {
	logger := d.Logger
	// enabled answers 501 when this console has no hooks configured.
	enabled := func(w http.ResponseWriter) (*spec.Platform, bool) {
		p := platform()
		if d.Hooks == nil || p == nil || !p.Hooks.Enabled() {
			http.Error(w, "hooks are not configured (platform.yaml hooks.publicURL)", http.StatusNotImplemented)
			return nil, false
		}
		return p, true
	}
	fail := func(w http.ResponseWriter, err error) {
		switch {
		case errors.Is(err, hooks.ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case strings.Contains(err.Error(), "already exists"):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		}
	}
	clean := func(events []string) []string {
		var out []string
		for _, e := range events {
			if e = strings.TrimSpace(e); e != "" && !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
		return out
	}

	mux.HandleFunc("POST /api/agents/{name}/hooks", func(w http.ResponseWriter, r *http.Request) {
		var body hookBody
		a, email, ok := editor(w, r, &body)
		if !ok {
			return
		}
		p, ok := enabled(w)
		if !ok {
			return
		}
		h := hooks.Hook{Agent: a.Name, Name: strings.TrimSpace(body.Name), Enabled: body.Enabled, Auth: body.Auth,
			Events: clean(body.Events), Prompt: body.Prompt, SettleSeconds: body.SettleSeconds, UpdatedBy: email, Updated: time.Now().UTC()}
		if err := h.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		// Settings first: Create fails atomically if the name is taken, so a
		// concurrent create cannot overwrite another hook's credential. Until
		// the credential lands, the hook rejects every delivery.
		if err := d.Hooks.Create(r.Context(), h); err != nil {
			fail(w, err)
			return
		}
		show, store := hooks.NewCredential(h.Auth)
		if err := d.Hooks.SetCredential(r.Context(), a.Name, h.Name, store); err != nil {
			logger.Error("set hook credential", "agent", a.Name, "hook", h.Name, "err", err)
			http.Error(w, "hook saved, but its credential could not be set; use New credential: "+err.Error(), http.StatusBadGateway)
			return
		}
		logger.Info("hook created", "user", email, "agent", a.Name, "hook", h.Name, "auth", h.Auth, "events", h.Events, "settle", h.SettleSeconds)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, hookCreated{Name: h.Name, URL: p.Hooks.URL(a.Name, h.Name), Auth: h.Auth, Secret: show})
	})

	mux.HandleFunc("PUT /api/agents/{name}/hooks/{hook}", func(w http.ResponseWriter, r *http.Request) {
		var body hookBody
		a, email, ok := editor(w, r, &body)
		if !ok {
			return
		}
		if _, ok := enabled(w); !ok {
			return
		}
		name := r.PathValue("hook")
		list, err := d.Hooks.List(r.Context(), a.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		i := slices.IndexFunc(list, func(x hooks.Hook) bool { return x.Name == name })
		if i < 0 {
			http.Error(w, hooks.ErrNotFound.Error(), http.StatusNotFound)
			return
		}
		// Name and auth are fixed; changing auth means a new credential.
		h := list[i]
		h.Enabled, h.Events, h.Prompt, h.SettleSeconds = body.Enabled, clean(body.Events), body.Prompt, body.SettleSeconds
		h.UpdatedBy, h.Updated = email, time.Now().UTC()
		if err := d.Hooks.Update(r.Context(), h); err != nil {
			fail(w, err)
			return
		}
		logger.Info("hook updated", "user", email, "agent", a.Name, "hook", h.Name, "enabled", h.Enabled, "events", h.Events, "settle", h.SettleSeconds)
		writeJSON(w, map[string]any{"name": h.Name, "enabled": h.Enabled})
	})

	mux.HandleFunc("POST /api/agents/{name}/hooks/{hook}/rotate", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		a, email, ok := editor(w, r, &body)
		if !ok {
			return
		}
		p, ok := enabled(w)
		if !ok {
			return
		}
		name := r.PathValue("hook")
		list, err := d.Hooks.List(r.Context(), a.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		i := slices.IndexFunc(list, func(x hooks.Hook) bool { return x.Name == name })
		if i < 0 {
			http.Error(w, hooks.ErrNotFound.Error(), http.StatusNotFound)
			return
		}
		show, store := hooks.NewCredential(list[i].Auth)
		if err := d.Hooks.SetCredential(r.Context(), a.Name, name, store); err != nil {
			logger.Error("rotate hook credential", "agent", a.Name, "hook", name, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		logger.Info("hook credential rotated", "user", email, "agent", a.Name, "hook", name)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, hookCreated{Name: name, URL: p.Hooks.URL(a.Name, name), Auth: list[i].Auth, Secret: show})
	})

	mux.HandleFunc("DELETE /api/agents/{name}/hooks/{hook}", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		a, email, ok := editor(w, r, &body)
		if !ok {
			return
		}
		if _, ok := enabled(w); !ok {
			return
		}
		name := r.PathValue("hook")
		if err := d.Hooks.Delete(r.Context(), a.Name, name); err != nil {
			fail(w, err)
			return
		}
		logger.Info("hook deleted", "user", email, "agent", a.Name, "hook", name)
		writeJSON(w, map[string]string{"deleted": name})
	})
}
