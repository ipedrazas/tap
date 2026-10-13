package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ipedrazas/tap/pkg/harness"
	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/s3"
	"github.com/ipedrazas/tap/pkg/spec"
	"github.com/ipedrazas/tap/pkg/version"
)

// Runs are the session traces every harness uploads to the sessions bucket
// (platform.yaml sessions) after each turn: <agent>/<session>.trace.json.
// The console reads them with a read-only key; only the agent's owning team
// may, because traces hold prompts and tool output.

const (
	runsPerPage = 25
	// maxListed bounds a listing: keys are random, so finding the newest
	// means listing them all.
	maxListed = 5000
	// headBytes is enough of a trace for its summary: the fields before the
	// first turn's prompt, and most prompts.
	headBytes = 16 << 10
	maxTrace  = 32 << 20
)

var runID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// runStore is the sessions bucket (pkg/s3.Client).
type runStore interface {
	List(ctx context.Context, prefix string, max int) ([]s3.Object, bool, error)
	Get(ctx context.Context, key string, max int64) ([]byte, error)
	Head(ctx context.Context, key string, n int64) ([]byte, error)
}

type runs struct {
	store runStore

	mu    sync.Mutex
	cache map[string]runSummary // by key; stale when the object changed
}

type runSummary struct {
	ID       string
	Updated  time.Time // last upload, i.e. the end of the last turn
	Size     int64
	Started  time.Time
	Customer string
	Version  string
	Trigger  string // of the first turn
	Prompt   string // of the first turn; empty when it did not fit the head
	Who      string
}

type runsPage struct {
	Agent     *inventory.Agent
	User      string
	Build     string
	Runs      []runSummary
	Page      int
	Next      bool
	Truncated bool
	Error     string
}

func newRuns(store runStore) *runs { return &runs{store: store, cache: map[string]runSummary{}} }

// list returns one page of an agent's runs, newest first.
func (rs *runs) list(ctx context.Context, agent string, page int) ([]runSummary, bool, bool, error) {
	objs, truncated, err := rs.store.List(ctx, agent+"/", maxListed)
	if err != nil {
		return nil, false, false, err
	}
	objs = slices.DeleteFunc(objs, func(o s3.Object) bool {
		id, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, agent+"/"), ".trace.json")
		return !ok || !runID.MatchString(id)
	})
	slices.SortFunc(objs, func(a, b s3.Object) int { return b.LastModified.Compare(a.LastModified) })
	start := page * runsPerPage
	if start >= len(objs) {
		return nil, false, truncated, nil
	}
	end := min(start+runsPerPage, len(objs))
	out := make([]runSummary, end-start)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, o := range objs[start:end] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = rs.summary(ctx, agent, o)
		}()
	}
	wg.Wait()
	return out, end < len(objs), truncated, nil
}

func (rs *runs) summary(ctx context.Context, agent string, o s3.Object) runSummary {
	rs.mu.Lock()
	s, ok := rs.cache[o.Key]
	rs.mu.Unlock()
	if ok && s.Updated.Equal(o.LastModified) && s.Size == o.Size {
		return s
	}
	id := strings.TrimSuffix(strings.TrimPrefix(o.Key, agent+"/"), ".trace.json")
	s = runSummary{ID: id, Updated: o.LastModified, Size: o.Size}
	b, err := rs.store.Head(ctx, o.Key, headBytes)
	if err != nil {
		return s // listed but unreadable: shown without details, not cached
	}
	parseHead(b, &s)
	rs.mu.Lock()
	if len(rs.cache) > 4*maxListed {
		clear(rs.cache)
	}
	rs.cache[o.Key] = s
	rs.mu.Unlock()
	return s
}

// parseHead reads a summary from the start of a trace document, which may
// be cut anywhere: it stops at the first turn's prompt or the first error.
func parseHead(b []byte, s *runSummary) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return
		}
		switch k {
		case "customer":
			_ = dec.Decode(&s.Customer)
		case "agent_version":
			_ = dec.Decode(&s.Version)
		case "start_time":
			_ = dec.Decode(&s.Started)
		case "turns":
			if t, err := dec.Token(); err != nil || t != json.Delim('[') || !dec.More() {
				return
			}
			if t, err := dec.Token(); err != nil || t != json.Delim('{') {
				return
			}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return
				}
				switch k {
				case "trigger":
					_ = dec.Decode(&s.Trigger)
				case "prompt":
					_ = dec.Decode(&s.Prompt)
					return
				default:
					var skip json.RawMessage
					if dec.Decode(&skip) != nil {
						return
					}
				}
			}
			return
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return
			}
		}
	}
}

func (rs *runs) get(ctx context.Context, agent, id string) ([]byte, *harness.Trace, error) {
	b, err := rs.store.Get(ctx, agent+"/"+id+".trace.json", maxTrace)
	if err != nil {
		return nil, nil, err
	}
	var t harness.Trace
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, nil, fmt.Errorf("trace is not a trace document: %w", err)
	}
	return b, &t, nil
}

// callers names the pseudonyms traces store (harness.UserKey) that this
// console can recognise: the viewer, team members and the agent's hooks.
func callers(p *spec.Platform, viewer string, hooks []string) map[string]string {
	names := map[string]string{}
	if p != nil {
		for _, members := range p.Teams {
			for _, m := range members {
				names[harness.UserKey(m)] = m
				names[harness.UserKey(strings.ToLower(m))] = m
			}
		}
	}
	for _, h := range hooks {
		names[harness.UserKey("hook:"+h)] = "hook " + h
	}
	if viewer != "" {
		names[harness.UserKey(viewer)] = "you"
	}
	return names
}

func who(names map[string]string, customer string) string {
	if n, ok := names[customer]; ok {
		return n
	}
	if customer == "" {
		return "unknown"
	}
	return "user " + customer[:min(8, len(customer))]
}

// runView is one trace as the run page shows it.
type runView struct {
	Agent   *inventory.Agent
	User    string
	Build   string
	Trace   *harness.Trace
	Who     string
	Tokens  harness.TraceTokens
	LLM     int
	Tools   int
	Size    int64
	Elapsed time.Duration
}

//go:embed runs.html
var runsHTML string

var runsPages = template.Must(template.New("runs").Funcs(template.FuncMap{
	"age":     func(t time.Time) string { return inventory.Age(t, time.Now()) },
	"when":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"elapsed": func(a, b time.Time) string { return b.Sub(a).Round(100 * time.Millisecond).String() },
	"ms": func(p *int64) string {
		if p == nil {
			return ""
		}
		return (time.Duration(*p) * time.Millisecond).Round(10 * time.Millisecond).String()
	},
	"kib":    func(n int64) string { return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KiB" },
	"pretty": pretty,
	"clip":   clip,
	"add":    func(a, b int) int { return a + b },
	"sub":    func(a, b int) int { return a - b },
}).Parse(runsHTML))

// pretty indents JSON; anything else is returned as is.
func pretty(v any) string {
	var b []byte
	switch x := v.(type) {
	case json.RawMessage:
		b = x
	case string:
		b = []byte(x)
	default:
		return fmt.Sprint(v)
	}
	var out bytes.Buffer
	if len(b) < 1<<20 && json.Indent(&out, b, "", "  ") == nil {
		return out.String()
	}
	return string(b)
}

// clip shortens s to n characters, saying how much was left out.
func clip(n int, s string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("\n… %d more characters (download the JSON for all of it)", len(r)-n)
}

func runRoutes(mux *http.ServeMux, d deps, list func(context.Context) ([]inventory.Agent, error), platform func() *spec.Platform) {
	logger := d.Logger
	// viewer admits a member of the agent's owning team.
	viewer := func(w http.ResponseWriter, r *http.Request) (*inventory.Agent, *spec.Platform, string, bool) {
		if d.Runs == nil {
			http.Error(w, "run history is not configured on this console (platform.yaml sessions and the sessions-reader key)", http.StatusNotImplemented)
			return nil, nil, "", false
		}
		a, err := find(r.Context(), list, r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return nil, nil, "", false
		}
		p, email := platform(), user(r)
		if !canEdit(p, email, a.Owner) {
			logger.Warn("runs refused", "user", email, "agent", a.Name, "owner", a.Owner)
			http.Error(w, "only members of team "+a.Owner+" can see this agent's runs", http.StatusForbidden)
			return nil, nil, "", false
		}
		return a, p, email, true
	}
	hookNames := func(ctx context.Context, agent string) []string {
		if d.Hooks == nil {
			return nil
		}
		hs, err := d.Hooks.List(ctx, agent)
		if err != nil {
			return nil
		}
		var names []string
		for _, h := range hs {
			names = append(names, h.Name)
		}
		return names
	}
	page := func(w http.ResponseWriter, name string, v any) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", pageCSP)
		w.Header().Set("Cache-Control", "no-store")
		if err := runsPages.ExecuteTemplate(w, name, v); err != nil {
			logger.Error("render", "err", err)
		}
	}

	mux.HandleFunc("GET /agents/{name}/runs", func(w http.ResponseWriter, r *http.Request) {
		a, p, email, ok := viewer(w, r)
		if !ok {
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n = max(n, 0)
		v := runsPage{Agent: a, User: email, Build: version.Get().Short(), Page: n}
		summaries, next, truncated, err := d.Runs.list(r.Context(), a.Name, n)
		if err != nil {
			logger.Error("list runs", "agent", a.Name, "err", err)
			v.Error = "Could not list this agent's runs from the sessions bucket."
		}
		names := callers(p, email, hookNames(r.Context(), a.Name))
		for i := range summaries {
			summaries[i].Who = who(names, summaries[i].Customer)
		}
		v.Runs, v.Next, v.Truncated = summaries, next, truncated
		page(w, "list", v)
	})

	mux.HandleFunc("GET /agents/{name}/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, p, email, ok := viewer(w, r)
		if !ok {
			return
		}
		id, asJSON := strings.CutSuffix(r.PathValue("id"), ".json")
		if !runID.MatchString(id) {
			http.Error(w, "bad run id", http.StatusNotFound)
			return
		}
		raw, t, err := d.Runs.get(r.Context(), a.Name, id)
		switch {
		case errors.Is(err, s3.ErrNotFound):
			http.Error(w, "no run "+id+" for "+a.Name, http.StatusNotFound)
			return
		case err != nil:
			logger.Error("read run", "agent", a.Name, "run", id, "err", err)
			http.Error(w, "could not read the run: "+err.Error(), http.StatusBadGateway)
			return
		}
		logger.Info("run viewed", "user", email, "agent", a.Name, "run", id, "json", asJSON)
		if asJSON {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", `attachment; filename="`+a.Name+"-"+id+`.trace.json"`)
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(raw)
			return
		}
		v := runView{Agent: a, User: email, Build: version.Get().Short(), Trace: t, Size: int64(len(raw)),
			Who: who(callers(p, email, hookNames(r.Context(), a.Name)), t.Customer), Elapsed: t.EndTime.Sub(t.StartTime)}
		for _, turn := range t.Turns {
			for _, s := range turn.Steps {
				switch {
				case s.LLMCall != nil:
					v.LLM++
					if tk := s.LLMCall.Tokens; tk != nil {
						v.Tokens.Input += tk.Input
						v.Tokens.Output += tk.Output
						v.Tokens.CacheRead += tk.CacheRead
					}
				case s.ToolCall != nil:
					v.Tools++
				}
			}
		}
		page(w, "run", v)
	})
}
