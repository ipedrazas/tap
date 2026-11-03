package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ipedrazas/tap/pkg/harness"
	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/s3"
	"github.com/ipedrazas/tap/pkg/spec"
	"github.com/ipedrazas/tap/pkg/verify"
	"github.com/ipedrazas/tap/pkg/version"
)

// Human review of verified answers (docs/acceptance.md). The harness writes
// one verification report per attempt to <agent>/<session>.verify.jsonl in
// the sessions bucket; the console reads them with its read-only key and
// lists the final ones that need a person (sampled by a human criterion, or
// out of attempts). A person's verdict is a verify.Review, written to a
// separate reviews bucket at <agent>/<session>/<turn>-<attempt>-<nanos>.json
// with the console's own write key: the console can't touch traces or
// reports, and reviews are never overwritten, so the dataset keeps every
// verdict and who gave it.

const (
	maxReportFile = 8 << 20
	maxReview     = 64 << 10
	// queueFiles bounds how many report files the queue reads (newest first).
	queueFiles = 200
)

var (
	turnID    = regexp.MustCompile(`^t[0-9]{1,4}$`)
	criterion = regexp.MustCompile(`^[a-z][a-z0-9-]{1,46}[a-z0-9]$`)
)

// reviewStore is the reviews bucket (pkg/s3.Client).
type reviewStore interface {
	Put(ctx context.Context, key, contentType string, body []byte) error
	List(ctx context.Context, prefix string, max int) ([]s3.Object, bool, error)
	Get(ctx context.Context, key string, max int64) ([]byte, error)
}

type reviews struct {
	store reviewStore
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedReports // report file key -> its final reports
}

type cachedReports struct {
	modified time.Time
	size     int64
	finals   []verify.Report
}

func newReviews(store reviewStore) *reviews {
	return &reviews{store: store, now: time.Now, cache: map[string]cachedReports{}}
}

func reviewKey(r verify.Review) string {
	return fmt.Sprintf("%s/%s/%s-%d-%d.json", r.Agent, r.SessionID, r.TurnID, r.Attempt, r.Time.UnixNano())
}

// put records a review; keys never repeat, so nothing is overwritten.
func (rv *reviews) put(ctx context.Context, r verify.Review) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return rv.store.Put(ctx, reviewKey(r), "application/json", body)
}

// forSession returns a session's reviews, oldest first.
func (rv *reviews) forSession(ctx context.Context, agent, session string) ([]verify.Review, error) {
	objs, _, err := rv.store.List(ctx, agent+"/"+session+"/", 1000)
	if err != nil {
		return nil, err
	}
	var out []verify.Review
	for _, o := range objs {
		b, err := rv.store.Get(ctx, o.Key, maxReview)
		if err != nil {
			return nil, err
		}
		var r verify.Review
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b verify.Review) int { return a.Time.Compare(b.Time) })
	return out, nil
}

// reviewed returns "<session>/<turn>" for every turn of agent that has a review.
func (rv *reviews) reviewed(ctx context.Context, agent string) (map[string]bool, error) {
	objs, _, err := rv.store.List(ctx, agent+"/", maxListed)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, agent+"/")
		session, file, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		turn, _, _ := strings.Cut(file, "-")
		out[session+"/"+turn] = true
	}
	return out, nil
}

// queueItem is one answer waiting for a person.
type queueItem struct {
	Report   verify.Report
	Failures []string
	Flagged  bool
	Updated  time.Time
}

// queue lists the final reports that need review and have none yet, newest
// first, from the newest queueFiles report files.
func (rv *reviews) queue(ctx context.Context, sessions runStore, agent string) ([]queueItem, bool, error) {
	objs, truncated, err := sessions.List(ctx, agent+"/", maxListed)
	if err != nil {
		return nil, false, err
	}
	objs = slices.DeleteFunc(objs, func(o s3.Object) bool {
		id, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, agent+"/"), ".verify.jsonl")
		return !ok || !runID.MatchString(id)
	})
	slices.SortFunc(objs, func(a, b s3.Object) int { return b.LastModified.Compare(a.LastModified) })
	if len(objs) > queueFiles {
		objs, truncated = objs[:queueFiles], true
	}
	done, err := rv.reviewed(ctx, agent)
	if err != nil {
		return nil, false, err
	}
	var out []queueItem
	for _, o := range objs {
		finals, err := rv.finals(ctx, sessions, o)
		if err != nil {
			return nil, false, err
		}
		for _, r := range finals {
			if !r.NeedsReview || done[r.SessionID+"/"+r.TurnID] {
				continue
			}
			item := queueItem{Report: r, Updated: o.LastModified}
			for _, c := range r.Failures() {
				item.Failures = append(item.Failures, c.ID)
			}
			item.Flagged = slices.ContainsFunc(r.Criteria, func(c verify.CriterionResult) bool { return c.Status == verify.Flagged })
			out = append(out, item)
		}
	}
	return out, truncated, nil
}

// finals reads a report file's final reports, cached until it changes.
func (rv *reviews) finals(ctx context.Context, sessions runStore, o s3.Object) ([]verify.Report, error) {
	rv.mu.Lock()
	c, ok := rv.cache[o.Key]
	rv.mu.Unlock()
	if ok && c.modified.Equal(o.LastModified) && c.size == o.Size {
		return c.finals, nil
	}
	b, err := sessions.Get(ctx, o.Key, maxReportFile)
	if err != nil {
		return nil, err
	}
	finals := parseReports(b, true)
	rv.mu.Lock()
	if len(rv.cache) > 4*queueFiles {
		clear(rv.cache)
	}
	rv.cache[o.Key] = cachedReports{modified: o.LastModified, size: o.Size, finals: finals}
	rv.mu.Unlock()
	return finals, nil
}

// parseReports reads JSONL reports, skipping lines that don't parse (a
// file caught mid-append). finalOnly keeps the last attempt of each turn.
func parseReports(b []byte, finalOnly bool) []verify.Report {
	var out []verify.Report
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxReportFile)
	for sc.Scan() {
		var r verify.Report
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Schema != verify.ReportSchema {
			continue
		}
		if !finalOnly || r.Final {
			out = append(out, r)
		}
	}
	return out
}

type reviewBody struct {
	TurnID   string            `json:"turn_id"`
	Attempt  int               `json:"attempt"`
	Verdict  string            `json:"verdict"`
	Criteria map[string]string `json:"criteria"`
	Note     string            `json:"note"`
}

func (b reviewBody) check(rep *verify.Report) error {
	switch {
	case b.Verdict != verify.Pass && b.Verdict != verify.Fail:
		return errors.New("verdict must be pass or fail")
	case utf8.RuneCountInString(b.Note) > 2000 || !utf8.ValidString(b.Note):
		return errors.New("note must be at most 2000 characters")
	case len(b.Criteria) > 32:
		return errors.New("at most 32 criteria")
	}
	for id, v := range b.Criteria {
		if !criterion.MatchString(id) || !slices.ContainsFunc(rep.Criteria, func(c verify.CriterionResult) bool { return c.ID == id }) {
			return fmt.Errorf("%q is not a criterion of this report", id)
		}
		if v != verify.Pass && v != verify.Fail {
			return fmt.Errorf("criterion %s: want pass or fail", id)
		}
	}
	return nil
}

// reportFor finds the report of one attempt of one turn in a trace.
func reportFor(t *harness.Trace, turn string, attempt int) *verify.Report {
	for _, tt := range t.Turns {
		if tt.ID != turn {
			continue
		}
		for _, r := range tt.Verification {
			if r.Attempt == attempt {
				return r
			}
		}
	}
	return nil
}

type reviewPage struct {
	Agent     *inventory.Agent
	User      string
	Build     string
	Items     []queueItem
	Truncated bool
	Error     string
}

func reviewRoutes(mux *http.ServeMux, d deps, list func(context.Context) ([]inventory.Agent, error), platform func() *spec.Platform, page func(http.ResponseWriter, string, any)) {
	logger := d.Logger
	// owner admits a member of the agent's owning team.
	owner := func(w http.ResponseWriter, r *http.Request) (*inventory.Agent, string, bool) {
		if d.Runs == nil || d.Reviews == nil {
			http.Error(w, "reviews are not configured on this console (platform.yaml sessions.reviewsBucket and the sessions-reviewer key)", http.StatusNotImplemented)
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
			logger.Warn("reviews refused", "user", email, "agent", a.Name, "owner", a.Owner)
			http.Error(w, "only members of team "+a.Owner+" can review this agent's runs", http.StatusForbidden)
			return nil, "", false
		}
		return a, email, true
	}

	mux.HandleFunc("GET /agents/{name}/review", func(w http.ResponseWriter, r *http.Request) {
		a, email, ok := owner(w, r)
		if !ok {
			return
		}
		v := reviewPage{Agent: a, User: email, Build: version.Get().Short()}
		items, truncated, err := d.Reviews.queue(r.Context(), d.Runs.store, a.Name)
		if err != nil {
			logger.Error("review queue", "agent", a.Name, "err", err)
			v.Error = "Could not read this agent's verification reports."
		}
		v.Items, v.Truncated = items, truncated
		page(w, "review", v)
	})

	mux.HandleFunc("POST /api/agents/{name}/runs/{id}/reviews", func(w http.ResponseWriter, r *http.Request) {
		// JSON only: a cross-site form can't send it without a preflight.
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
			http.Error(w, "want application/json", http.StatusUnsupportedMediaType)
			return
		}
		a, email, ok := owner(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if !runID.MatchString(id) {
			http.Error(w, "bad run id", http.StatusNotFound)
			return
		}
		var body reviewBody
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReview))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !turnID.MatchString(body.TurnID) || body.Attempt < 1 || body.Attempt > 5 {
			http.Error(w, "want turn_id t<n> and attempt 1-5", http.StatusBadRequest)
			return
		}
		_, t, err := d.Runs.get(r.Context(), a.Name, id)
		switch {
		case errors.Is(err, s3.ErrNotFound):
			http.Error(w, "no run "+id+" for "+a.Name, http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, "could not read the run: "+err.Error(), http.StatusBadGateway)
			return
		}
		rep := reportFor(t, body.TurnID, body.Attempt)
		if rep == nil {
			http.Error(w, "turn "+body.TurnID+" has no verification report for attempt "+strconv.Itoa(body.Attempt), http.StatusNotFound)
			return
		}
		if err := body.check(rep); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		rv := verify.Review{
			Schema: verify.ReviewSchema, Agent: a.Name, SessionID: id, TurnID: body.TurnID, Attempt: body.Attempt,
			Reviewer: email, Time: d.Reviews.now().UTC(), Verdict: body.Verdict, VerifierStatus: rep.Status,
			Criteria: body.Criteria, Note: strings.TrimSpace(body.Note),
		}
		if err := d.Reviews.put(r.Context(), rv); err != nil {
			logger.Error("write review", "agent", a.Name, "run", id, "err", err)
			http.Error(w, "could not save the review", http.StatusBadGateway)
			return
		}
		logger.Info("run reviewed", "user", email, "agent", a.Name, "run", id, "turn", body.TurnID, "attempt", body.Attempt, "verdict", rv.Verdict, "verifier", rep.Status)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, rv)
	})
}
