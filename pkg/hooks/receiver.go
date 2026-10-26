package hooks

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxBody bounds a delivery. GitHub payloads for large pull requests are a
// few hundred KiB.
const MaxBody = 1 << 20

// TriggerRequest is what tap-hooks sends to a harness's trigger port.
type TriggerRequest struct {
	Hook     string `json:"hook"`
	Delivery string `json:"delivery,omitempty"`
	Event    string `json:"event,omitempty"`
	Prompt   string `json:"prompt"`
}

// TriggerResponse is the harness's answer: the session the run lives in.
type TriggerResponse struct {
	SessionID string `json:"session_id"`
}

// ErrBusy means the agent is already running as many hook turns as it allows.
var ErrBusy = errors.New("agent is busy")

// Receiver is tap-hooks' public handler: POST /h/{agent}/{hook}.
type Receiver struct {
	Lookup  func(ctx context.Context, agent, name string) (*Entry, error)
	Trigger func(ctx context.Context, agent string, req TriggerRequest) (*TriggerResponse, error)
	Logger  *slog.Logger
	// PerMinute caps accepted deliveries per hook (default 30).
	PerMinute int
	// MaxReading caps bodies being read at once, before their signature is
	// checked (default 8): unauthenticated senders can hold at most
	// MaxReading*MaxBody of memory.
	MaxReading int

	reading chan struct{}
	mu      sync.Mutex
	seen    map[string]time.Time
	counts  map[string]*window
	pending map[string]*pendingRun
	// Tests shorten these: the unit of SettleSeconds and the wait before a
	// held run is tried again.
	settleUnit, retryDelay time.Duration
}

type window struct {
	start time.Time
	n     int
}

func (rc *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /h/{agent}/{hook}", rc.receive)
	return mux
}

func (rc *Receiver) startReading() bool {
	rc.mu.Lock()
	if rc.reading == nil {
		rc.reading = make(chan struct{}, cmp.Or(rc.MaxReading, 8))
	}
	slots := rc.reading
	rc.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (rc *Receiver) doneReading() { <-rc.reading }

func reply(w http.ResponseWriter, status int, v map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (rc *Receiver) receive(w http.ResponseWriter, r *http.Request) {
	agent, name := r.PathValue("agent"), r.PathValue("hook")
	log := rc.Logger.With("agent", agent, "hook", name)
	if !AgentRE.MatchString(agent) || !NameRE.MatchString(name) {
		reply(w, http.StatusNotFound, map[string]string{"error": "no such hook"})
		return
	}
	// Look the hook up before reading up to MaxBody of an unauthenticated body.
	e, err := rc.Lookup(r.Context(), agent, name)
	if errors.Is(err, ErrNotFound) || (err == nil && !e.Enabled) {
		// Unknown and disabled hooks look the same to the sender.
		reply(w, http.StatusNotFound, map[string]string{"error": "no such hook"})
		return
	}
	if err != nil {
		log.Error("lookup", "err", err)
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "hooks are unavailable"})
		return
	}
	if r.ContentLength > MaxBody {
		reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return
	}
	if !rc.startReading() {
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "too many deliveries in progress"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	rc.doneReading()
	if err != nil {
		reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return
	}
	if err := Verify(e.Auth, e.Credential, r.Header, body); err != nil {
		log.Warn("unauthorized delivery", "remote", r.Header.Get("X-Forwarded-For"))
		reply(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	d, err := ParseDelivery(r.Header, body)
	if err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log = log.With("event", d.Event, "action", d.Action, "delivery", d.ID)
	if d.Event == "ping" {
		log.Info("ping")
		reply(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	ds, err := d.Expand()
	if err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(ds) == 1 && ds[0] == d {
		res := rc.handle(r.Context(), log, e, d, body)
		if res.retryAfter != "" {
			w.Header().Set("Retry-After", res.retryAfter)
		}
		reply(w, res.code, res.out)
		return
	}
	// A batch: the sender retries all of it on any failure, and the events
	// that already started or were scheduled come back as duplicates.
	counts := map[string]int{}
	worst := result{code: http.StatusOK, out: map[string]string{}}
	for _, x := range ds {
		res := rc.handle(r.Context(), log.With("event", x.Event, "action", x.Action, "delivery", x.ID, "key", x.Object.Key), e, x, nil)
		counts[cmp.Or(res.out["status"], "failed")]++
		switch {
		case res.code >= 400 && (worst.code < 400 || res.code > worst.code):
			worst = res
		case res.code == http.StatusAccepted && worst.code == http.StatusOK:
			worst.code = http.StatusAccepted
		}
	}
	if worst.retryAfter != "" {
		w.Header().Set("Retry-After", worst.retryAfter)
	}
	out := map[string]string{}
	for k, v := range worst.out {
		out[k] = v
	}
	if worst.code < 400 {
		out = map[string]string{"status": "batch"}
	}
	for k, n := range counts {
		out[k] = strconv.Itoa(n)
	}
	reply(w, worst.code, out)
}

// result is the reply for one delivery.
type result struct {
	code       int
	out        map[string]string
	retryAfter string
}

// handle runs one verified delivery. body, when set, is deduplicated by
// hash as well as by ID: the ID header isn't signed. Deliveries split from a
// batch pass nil; their ID comes from the signed body.
func (rc *Receiver) handle(ctx context.Context, log *slog.Logger, e *Entry, d *Delivery, body []byte) result {
	if !e.Wants(d) {
		log.Info("delivery ignored by the event filter")
		return result{code: http.StatusOK, out: map[string]string{"status": "ignored", "reason": "event not subscribed"}}
	}
	prompt, err := e.Render(d)
	if errors.Is(err, ErrEmptyPrompt) {
		log.Info("delivery ignored: the prompt rendered empty")
		return result{code: http.StatusOK, out: map[string]string{"status": "ignored", "reason": "prompt rendered empty"}}
	}
	if err != nil {
		log.Warn("render prompt", "err", err)
		return result{code: http.StatusUnprocessableEntity, out: map[string]string{"error": "prompt template: " + err.Error()}}
	}
	var keys []string
	if body != nil {
		sum := sha256.Sum256(body)
		keys = append(keys, e.Key()+"/body/"+hex.EncodeToString(sum[:]))
	}
	if d.ID != "" {
		keys = append(keys, e.Key()+"/id/"+d.ID)
	}
	if !rc.reserve(keys) {
		return result{code: http.StatusOK, out: map[string]string{"status": "duplicate", "delivery": d.ID}}
	}
	req := TriggerRequest{Hook: e.Name, Delivery: d.ID, Event: d.Event, Prompt: prompt}
	if e.SettleSeconds > 0 && d.Object != nil {
		after := time.Duration(e.SettleSeconds) * cmp.Or(rc.settleUnit, time.Second)
		if !rc.schedule(e.Key()+"/obj/"+d.Object.Bucket+"/"+d.Object.Key, e, req, after, log) {
			rc.release(keys)
			log.Warn("too many scheduled runs")
			return result{code: http.StatusServiceUnavailable, out: map[string]string{"error": "too many scheduled runs, retry later"}, retryAfter: "60"}
		}
		log.Info("run scheduled", "settle", after)
		return result{code: http.StatusAccepted, out: map[string]string{"status": "scheduled", "delivery": d.ID}}
	}
	started := false
	defer func() {
		if !started {
			rc.release(keys)
		}
	}()
	if !rc.allow(e.Key()) {
		log.Warn("rate limited")
		return result{code: http.StatusTooManyRequests, out: map[string]string{"error": "too many deliveries for this hook"}, retryAfter: "60"}
	}
	res, err := rc.Trigger(ctx, e.Agent, req)
	switch {
	case errors.Is(err, ErrBusy):
		log.Warn("agent busy")
		return result{code: http.StatusServiceUnavailable, out: map[string]string{"error": "agent is busy, retry later"}, retryAfter: "30"}
	case err != nil:
		log.Error("trigger", "err", err)
		return result{code: http.StatusBadGateway, out: map[string]string{"error": "the agent is unavailable"}}
	}
	started = true
	log.Info("run started", "session", res.SessionID)
	return result{code: http.StatusAccepted, out: map[string]string{"status": "started", "session_id": res.SessionID, "delivery": d.ID}}
}

// A scheduled run is held until its object has been quiet for the hook's
// settle time. Pending runs live in memory: a restart drops them (main logs
// how many), and the sender, which got a 202, won't send them again.
const (
	maxPending = 10000
	maxTries   = 20
)

type pendingRun struct {
	hook  string // Entry.Key, for the rate limit
	agent string
	req   TriggerRequest
	log   *slog.Logger
	timer *time.Timer
	tries int
}

// schedule holds req under key, replacing an older one and restarting its
// clock. It reports false when too many runs are pending.
func (rc *Receiver) schedule(key string, e *Entry, req TriggerRequest, after time.Duration, log *slog.Logger) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.pending == nil {
		rc.pending = map[string]*pendingRun{}
	}
	if p, ok := rc.pending[key]; ok {
		p.req, p.log, p.tries = req, log, 0
		p.timer.Reset(after)
		return true
	}
	if len(rc.pending) >= maxPending {
		return false
	}
	p := &pendingRun{hook: e.Key(), agent: e.Agent, req: req, log: log}
	p.timer = time.AfterFunc(after, func() { rc.fire(key, p) })
	rc.pending[key] = p
	return true
}

// fire starts a held run, or holds it again when the agent is busy, the
// hook is over its rate limit or the trigger fails.
func (rc *Receiver) fire(key string, p *pendingRun) {
	rc.mu.Lock()
	if rc.pending[key] != p {
		rc.mu.Unlock() // superseded, or already fired
		return
	}
	delete(rc.pending, key)
	req := p.req
	rc.mu.Unlock()
	log := p.log
	var err error
	if rc.allow(p.hook) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var res *TriggerResponse
		res, err = rc.Trigger(ctx, p.agent, req)
		cancel()
		if err == nil {
			log.Info("run started", "session", res.SessionID)
			return
		}
	} else {
		err = errors.New("rate limited")
	}
	p.tries++
	if p.tries >= maxTries {
		log.Error("scheduled run dropped", "tries", p.tries, "err", err)
		return
	}
	log.Warn("scheduled run held again", "tries", p.tries, "err", err)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if _, newer := rc.pending[key]; !newer {
		rc.pending[key] = p
		p.timer = time.AfterFunc(cmp.Or(rc.retryDelay, 30*time.Second), func() { rc.fire(key, p) })
	}
}

// Pending is how many scheduled runs are waiting.
func (rc *Receiver) Pending() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.pending)
}

// Deliveries are remembered for a day: GitHub redeliveries reuse the ID.
const dedupTTL = 24 * time.Hour

// reserve claims every key, or none if any was seen within dedupTTL.
// Concurrent redeliveries cannot both pass; release undoes a failed run.
func (rc *Receiver) reserve(keys []string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.seen == nil {
		rc.seen = map[string]time.Time{}
	}
	now := time.Now()
	for _, k := range keys {
		if t, ok := rc.seen[k]; ok && now.Sub(t) < dedupTTL {
			return false
		}
	}
	if len(rc.seen) > 20000 {
		for k, t := range rc.seen {
			if now.Sub(t) >= dedupTTL {
				delete(rc.seen, k)
			}
		}
	}
	for _, k := range keys {
		rc.seen[k] = now
	}
	return true
}

func (rc *Receiver) release(keys []string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, k := range keys {
		delete(rc.seen, k)
	}
}

// allow is a fixed one-minute window per hook.
func (rc *Receiver) allow(key string) bool {
	limit := rc.PerMinute
	if limit <= 0 {
		limit = 30
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.counts == nil {
		rc.counts = map[string]*window{}
	}
	now := time.Now()
	c := rc.counts[key]
	if c == nil || now.Sub(c.start) >= time.Minute {
		c = &window{start: now}
		rc.counts[key] = c
	}
	if c.n >= limit {
		return false
	}
	c.n++
	return true
}

// HarnessTrigger calls POST /v1/trigger on an agent's trigger port. URL is a
// pattern with {agent}, e.g. http://agent.agent-{agent}.svc.cluster.local:8081.
// Token, the platform-wide trigger token, is mounted into tap-hooks and each
// harness only, so tool code that shares the harness's loopback cannot call
// the trigger port.
type HarnessTrigger struct {
	URL    string
	Token  string
	Client *http.Client
}

func (t *HarnessTrigger) Trigger(ctx context.Context, agent string, req TriggerRequest) (*TriggerResponse, error) {
	body, _ := json.Marshal(req)
	u := strings.ReplaceAll(t.URL, "{agent}", agent) + "/v1/trigger"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+t.Token)
	resp, err := t.Client.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
		var out TriggerResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out); err != nil {
			return nil, err
		}
		return &out, nil
	case http.StatusTooManyRequests:
		return nil, ErrBusy
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return nil, fmt.Errorf("harness: %s %s", strconv.Itoa(resp.StatusCode), strings.TrimSpace(string(msg)))
}
