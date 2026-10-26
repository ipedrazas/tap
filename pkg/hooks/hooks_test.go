package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerify(t *testing.T) {
	body := `{"action":"opened"}`
	show, stored := NewCredential(AuthGitHub)
	if show != stored || len(show) != 64 {
		t.Fatalf("github secret %q stored as %q", show, stored)
	}
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(show, body))
	if err := Verify(AuthGitHub, stored, h, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if Verify(AuthGitHub, stored, h, []byte(body+" ")) == nil {
		t.Error("tampered body accepted")
	}
	h.Set("X-Hub-Signature-256", sign("other", body))
	if Verify(AuthGitHub, stored, h, []byte(body)) == nil {
		t.Error("wrong secret accepted")
	}
	if Verify(AuthGitHub, "", http.Header{}, nil) == nil {
		t.Error("missing credential accepted")
	}

	token, stored := NewCredential(AuthBearer)
	if !strings.HasPrefix(token, TokenPrefix) || strings.Contains(stored, token) || !strings.HasPrefix(stored, "sha256:") {
		t.Fatalf("bearer token %q stored as %q", token, stored)
	}
	h = http.Header{}
	h.Set("Authorization", "Bearer "+token)
	if err := Verify(AuthBearer, stored, h, nil); err != nil {
		t.Fatal(err)
	}
	h.Set("Authorization", "Bearer "+token+"x")
	if Verify(AuthBearer, stored, h, nil) == nil {
		t.Error("wrong token accepted")
	}
	// A github hook never accepts a bearer token, even its own secret.
	h.Set("Authorization", "Bearer "+show)
	if Verify(AuthGitHub, show, h, nil) == nil {
		t.Error("bearer accepted on a github hook")
	}
}

func TestValidate(t *testing.T) {
	ok := Hook{Agent: "gh-pr-agent", Name: "prs", Auth: AuthGitHub, Events: []string{"pull_request.opened", "push"}, Prompt: "Review {{.payload.pull_request.html_url}}"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Hook){
		"name":     func(h *Hook) { h.Name = "Bad_Name" },
		"auth":     func(h *Hook) { h.Auth = "none" },
		"event":    func(h *Hook) { h.Events = []string{"pull request"} },
		"empty":    func(h *Hook) { h.Prompt = "  " },
		"template": func(h *Hook) { h.Prompt = "{{.payload" },
		"func":     func(h *Hook) { h.Prompt = `{{exec "ls"}}` },
		"define":   func(h *Hook) { h.Prompt = `{{define "a"}}x{{end}}{{template "a"}}` },
		"block":    func(h *Hook) { h.Prompt = `{{block "a" .}}x{{end}}` },
		"nested":   func(h *Hook) { h.Prompt = `{{range .payload.a}}{{if .}}{{range $.payload.a}}x{{end}}{{end}}{{end}}` },
		// Range over an integer loops without writing output.
		"range int":     func(h *Hook) { h.Prompt = `{{range 300000000}}{{end}}` },
		"range var":     func(h *Hook) { h.Prompt = `{{$n := 300000000}}{{range $n}}{{end}}` },
		"range with":    func(h *Hook) { h.Prompt = `{{with 300000000}}{{range .}}{{end}}{{end}}` },
		"range or":      func(h *Hook) { h.Prompt = `{{range or 0 300000000}}{{end}}` },
		"range paren":   func(h *Hook) { h.Prompt = `{{range (300000000)}}{{end}}` },
		"range len":     func(h *Hook) { h.Prompt = `{{range len .payload.a}}{{end}}` },
		"if number":     func(h *Hook) { h.Prompt = `{{if eq .payload.n 1}}x{{end}}` },
		"len in parens": func(h *Hook) { h.Prompt = `{{truncate (len .payload.a) .payload.b}}` },
	} {
		h := ok
		mut(&h)
		if h.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestValidateAllowsNumberArgs(t *testing.T) {
	for _, p := range []string{`{{index .payload.list 0}}`, `{{range slice .payload.list 1 3}}{{.}}{{end}}`, `{{truncate 80 .payload.title}}`, `{{with index .payload.list 0}}{{.name}}{{end}}`, `{{len .payload.list}} items`} {
		h := Hook{Agent: "a1", Name: "prs", Auth: AuthGitHub, Prompt: p}
		if err := h.Validate(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestRender(t *testing.T) {
	h := &Hook{Agent: "a1", Name: "prs", Events: []string{"pull_request.opened", "issues"},
		Prompt: `{{.event}}/{{.action}} on {{.payload.repository.full_name}}#{{.payload.number}}{{.payload.missing}}{{with .payload.gone}}{{.deep}}{{end}} {{truncate 3 .payload.title}} {{json .payload.labels}}`}
	header := http.Header{}
	header.Set("X-GitHub-Event", "pull_request")
	header.Set("X-GitHub-Delivery", "d-1")
	d, err := ParseDelivery(header, []byte(`{"action":"opened","number":12345678901,"title":"abcdef","labels":["x"],"repository":{"full_name":"o/r"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !h.Wants(d) || d.ID != "d-1" {
		t.Fatalf("delivery %+v not wanted", d)
	}
	out, err := h.Render(d)
	if err != nil {
		t.Fatal(err)
	}
	if want := `pull_request/opened on o/r#12345678901 abc… ["x"]`; out != want {
		t.Fatalf("render\n got %q\nwant %q", out, want)
	}
	deep := &Hook{Prompt: `{{.payload.missing.deep}}`}
	if _, err := deep.Render(d); err == nil {
		t.Error("a field under a missing one must fail loudly")
	}
	d.Action = "closed"
	if h.Wants(d) {
		t.Error("closed is not subscribed")
	}
	d.Event, d.Action = "issues", "anything"
	if !h.Wants(d) {
		t.Error("issues takes every action")
	}
	if _, err := ParseDelivery(header, []byte("payload=%7B%7D")); err == nil {
		t.Error("form-encoded body accepted")
	}
	printf := &Hook{Prompt: `{{printf "%999999999s" "x"}}`}
	if _, err := printf.Render(d); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("printf: %v", err)
	}
	big := &Hook{Prompt: `{{range .payload}}{{.}}{{end}}`}
	d.Payload = []any{strings.Repeat("x", MaxRendered+1)}
	if _, err := big.Render(d); err == nil {
		t.Error("oversized prompt accepted")
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	c := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: "tap-system"}, Data: map[string]string{"a1.title": "A1"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: Secret, Namespace: "tap-system"}},
	)
	return &Store{C: c, Namespace: "tap-system"}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := Hook{Agent: "a1", Name: "prs", Enabled: true, Auth: AuthGitHub, Prompt: "go"}
	if err := s.Create(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, h); err == nil {
		t.Fatal("duplicate created")
	}
	if err := s.SetCredential(ctx, "a1", "prs", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, Hook{Agent: "a2", Name: "other", Auth: AuthBearer, Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "a1")
	if err != nil || len(list) != 1 || list[0].Name != "prs" {
		t.Fatalf("list %v %+v", err, list)
	}
	all, err := s.Load(ctx)
	if err != nil || len(all) != 2 || all["a1.prs"].Credential != "s3cret" || all["a2.other"].Credential != "" {
		t.Fatalf("load %v %+v", err, all)
	}
	h.Enabled = false
	if err := s.Update(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, Hook{Agent: "a1", Name: "nope", Auth: AuthGitHub, Prompt: "x"}); err != ErrNotFound {
		t.Fatalf("update missing: %v", err)
	}
	if err := s.Delete(ctx, "a1", "prs"); err != nil {
		t.Fatal(err)
	}
	all, _ = s.Load(ctx)
	sec, _ := s.C.CoreV1().Secrets("tap-system").Get(ctx, Secret, metav1.GetOptions{})
	if _, ok := all["a1.prs"]; ok || len(sec.Data) != 0 {
		t.Fatalf("delete left %+v %v", all, sec.Data)
	}
	cm, _ := s.C.CoreV1().ConfigMaps("tap-system").Get(ctx, ConfigMap, metav1.GetOptions{})
	if cm.Data["a1.title"] != "A1" {
		t.Error("titles must survive hook writes")
	}
}

func TestCache(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Create(ctx, Hook{Agent: "a1", Name: "prs", Auth: AuthBearer, Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	// Each ConfigMap read waits for a value on reads; nil lets it through.
	reads := make(chan error)
	var hold atomic.Bool
	s.C.(*fake.Clientset).PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !hold.Load() {
			return false, nil, nil
		}
		if err := <-reads; err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	c := &Cache{Store: s, TTL: 0, MaxStale: time.Hour}

	// A sender that hangs up mid-load doesn't fail the load for everyone.
	hold.Store(true)
	gone, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { _, err := c.Lookup(gone, "a1", "prs"); done <- err }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup: %v", err)
	}
	reads <- nil
	hold.Store(false)
	if _, err := c.Lookup(ctx, "a1", "prs"); err != nil {
		t.Fatalf("after a cancelled lookup: %v", err)
	}

	// Reloads fail: the last snapshot keeps serving, up to MaxStale.
	hold.Store(true)
	go func() { reads <- errors.New("apiserver down") }()
	if _, err := c.Lookup(ctx, "a1", "prs"); err != nil {
		t.Fatalf("stale snapshot not served: %v", err)
	}
	if _, err := c.Lookup(ctx, "a1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hook on a stale snapshot: %v", err)
	}
	c.MaxStale = time.Nanosecond
	if _, err := c.Lookup(ctx, "a1", "prs"); err == nil {
		t.Fatal("served a snapshot older than MaxStale")
	}
}

func TestReceiverBoundsReads(t *testing.T) {
	rc := &Receiver{
		Lookup: func(context.Context, string, string) (*Entry, error) {
			return &Entry{Hook: Hook{Enabled: true, Auth: AuthBearer}}, nil
		},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxReading: 1,
	}
	srv := httptest.NewServer(rc.Handler())
	defer srv.Close()
	// One slow upload holds the only read slot.
	pr, pw := io.Pipe()
	defer pw.Close()
	go http.Post(srv.URL+"/h/a1/prs", "application/json", pr)
	pw.Write([]byte("{"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Post(srv.URL+"/h/a1/prs", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second read: %d, want 503", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReceiver(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	secret, stored := NewCredential(AuthGitHub)
	if err := s.Create(ctx, Hook{Agent: "a1", Name: "prs", Enabled: true, Auth: AuthGitHub, Events: []string{"pull_request.opened"}, Prompt: "Review {{.payload.pull_request.html_url}}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredential(ctx, "a1", "prs", stored); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, Hook{Agent: "a1", Name: "off", Enabled: false, Auth: AuthGitHub, Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	s.SetCredential(ctx, "a1", "off", stored)

	var got []TriggerRequest
	harness := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req TriggerRequest
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/v1/trigger" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("trigger at %s with %q", r.URL, r.Header.Get("Authorization"))
		}
		got = append(got, req)
		if len(got) > 2 {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"session_id":"s1"}`)
	}))
	defer harness.Close()
	cache := &Cache{Store: s, TTL: time.Minute}
	trig := &HarnessTrigger{URL: harness.URL, Token: "tok", Client: http.DefaultClient}
	rc := &Receiver{Lookup: cache.Lookup, Trigger: trig.Trigger, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PerMinute: 3}
	srv := httptest.NewServer(rc.Handler())
	defer srv.Close()

	post := func(path, event, delivery, body, sig string) (int, map[string]string) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", delivery)
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	opened := `{"action":"opened","pull_request":{"html_url":"https://github.com/o/r/pull/1"}}`
	opened2 := `{"action":"opened","pull_request":{"html_url":"https://github.com/o/r/pull/2"}}`
	opened3 := `{"action":"opened","pull_request":{"html_url":"https://github.com/o/r/pull/3"}}`
	opened4 := `{"action":"opened","pull_request":{"html_url":"https://github.com/o/r/pull/4"}}`
	for _, c := range []struct {
		name, path, event, delivery, body, sig string
		code                                   int
		status                                 string
	}{
		{"unknown hook", "/h/a1/nope", "pull_request", "1", opened, sign(secret, opened), 404, ""},
		{"disabled hook", "/h/a1/off", "pull_request", "1", opened, sign(secret, opened), 404, ""},
		{"bad path", "/h/A1/prs", "pull_request", "1", opened, sign(secret, opened), 404, ""},
		{"unsigned", "/h/a1/prs", "pull_request", "1", opened, "", 401, ""},
		{"bad signature", "/h/a1/prs", "pull_request", "1", opened, sign("x", opened), 401, ""},
		{"ping", "/h/a1/prs", "ping", "p", `{"zen":"hi"}`, sign(secret, `{"zen":"hi"}`), 200, "pong"},
		{"filtered", "/h/a1/prs", "pull_request", "2", `{"action":"closed"}`, sign(secret, `{"action":"closed"}`), 200, "ignored"},
		{"started", "/h/a1/prs", "pull_request", "3", opened, sign(secret, opened), 202, "started"},
		{"duplicate", "/h/a1/prs", "pull_request", "3", opened, sign(secret, opened), 200, "duplicate"},
		{"replayed body, new id", "/h/a1/prs", "pull_request", "3b", opened, sign(secret, opened), 200, "duplicate"},
		{"second", "/h/a1/prs", "pull_request", "4", opened2, sign(secret, opened2), 202, "started"},
		{"busy", "/h/a1/prs", "pull_request", "5", opened3, sign(secret, opened3), 503, ""},
		{"rate limited", "/h/a1/prs", "pull_request", "6", opened4, sign(secret, opened4), 429, ""},
	} {
		code, out := post(c.path, c.event, c.delivery, c.body, c.sig)
		if code != c.code || out["status"] != c.status {
			t.Errorf("%s: %d %v, want %d %q", c.name, code, out, c.code, c.status)
		}
	}
	if len(got) != 3 || got[0].Prompt != "Review https://github.com/o/r/pull/1" || got[0].Hook != "prs" || got[0].Delivery != "3" {
		t.Fatalf("triggers %+v", got)
	}
	// A busy agent does not mark the delivery as seen, so a redelivery runs.
	if !rc.reserve([]string{"a1.prs/id/5"}) {
		t.Error("failed delivery remembered")
	}
}

func tigrisEvent(name, key, etag string) string {
	return `{"eventVersion":"1","eventSource":"tigris","eventName":"` + name + `","eventTime":"2026-10-26T12:00:00Z","bucket":"sessions","object":{"key":"` + key + `","size":1024,"eTag":"` + etag + `"}}`
}

func tigrisBatch(events ...string) string { return `{"events":[` + strings.Join(events, ",") + `]}` }

func TestExpand(t *testing.T) {
	parse := func(body string) *Delivery {
		d, err := ParseDelivery(http.Header{}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	ds, err := parse(tigrisBatch(tigrisEvent("OBJECT_CREATED_PUT", "a/1.json", "e1"), tigrisEvent("OBJECT_DELETED", "a/2.json", "e2"))).Expand()
	if err != nil || len(ds) != 2 {
		t.Fatalf("%v %v", ds, err)
	}
	if d := ds[0]; d.Event != "object_created" || d.Action != "put" || d.Object.Key != "a/1.json" || d.Object.Bucket != "sessions" || d.Object.Size.String() != "1024" || !strings.HasPrefix(d.ID, "tigris:") {
		t.Errorf("created: %+v %+v", d, d.Object)
	}
	if d := ds[1]; d.Event != "object_deleted" || d.Action != "" {
		t.Errorf("deleted: %+v", d)
	}
	// A new version of the same object is a new delivery; a redelivery is not.
	again, _ := parse(tigrisBatch(tigrisEvent("OBJECT_CREATED_PUT", "a/1.json", "e1"))).Expand()
	newer, _ := parse(tigrisBatch(tigrisEvent("OBJECT_CREATED_PUT", "a/1.json", "e9"))).Expand()
	if again[0].ID != ds[0].ID || newer[0].ID == ds[0].ID {
		t.Errorf("ids: %s %s %s", ds[0].ID, again[0].ID, newer[0].ID)
	}
	// Anything else is one delivery, as before.
	for _, body := range []string{`{"action":"opened"}`, `{"events":[{"eventSource":"github"}]}`, `{"events":[]}`, `[1,2]`} {
		d := parse(body)
		if ds, err := d.Expand(); err != nil || len(ds) != 1 || ds[0] != d {
			t.Errorf("%s: %v %v", body, ds, err)
		}
	}
	for _, body := range []string{
		tigrisBatch(`{"eventSource":"tigris","eventName":"OBJECT_CREATED_PUT","bucket":"b","object":{}}`),
		tigrisBatch(`{"eventSource":"tigris","eventName":"rm -rf","bucket":"b","object":{"key":"k"}}`),
		tigrisBatch(strings.Repeat(tigrisEvent("OBJECT_DELETED", "k", "e")+",", MaxBatch) + tigrisEvent("OBJECT_DELETED", "k", "e")),
	} {
		if _, err := parse(body).Expand(); err == nil {
			t.Errorf("accepted %.80s", body)
		}
	}
}

func TestRenderObject(t *testing.T) {
	h := Hook{Agent: "a1", Name: "sessions", Auth: AuthBearer, Events: []string{"object_created"}, SettleSeconds: 600,
		Prompt: `{{with .object}}{{if hasPrefix .key "gh-pr-agent/"}}Analyse t3://{{.bucket}}/{{.key}} ({{.size}} bytes).{{end}}{{end}}`}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	d, _ := ParseDelivery(http.Header{}, []byte(tigrisBatch(tigrisEvent("OBJECT_CREATED_PUT", "gh-pr-agent/x.trace.json", "e1"), tigrisEvent("OBJECT_CREATED_PUT", "other/y.json", "e2"))))
	ds, _ := d.Expand()
	if got, err := h.Render(ds[0]); err != nil || got != "Analyse t3://sessions/gh-pr-agent/x.trace.json (1024 bytes)." {
		t.Errorf("registered: %q %v", got, err)
	}
	if _, err := h.Render(ds[1]); !errors.Is(err, ErrEmptyPrompt) {
		t.Errorf("unregistered: %v", err)
	}
	for _, s := range []int{-1, MaxSettle + 1} {
		bad := h
		bad.SettleSeconds = s
		if bad.Validate() == nil {
			t.Errorf("settle %d accepted", s)
		}
	}
}

// objectHarness answers 429 for the first `busy` triggers, then 202.
func objectHarness(t *testing.T, busy int) (*httptest.Server, func() []TriggerRequest) {
	var mu sync.Mutex
	var got []TriggerRequest
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req TriggerRequest
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= busy {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		got = append(got, req)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"session_id":"s`+strconv.Itoa(len(got))+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []TriggerRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func objectReceiver(t *testing.T, h Hook, harness string) (*Receiver, func(body string) (int, map[string]string)) {
	token, stored := NewCredential(AuthBearer)
	rc := &Receiver{
		Lookup: func(context.Context, string, string) (*Entry, error) {
			return &Entry{Hook: h, Credential: stored}, nil
		},
		Trigger:    (&HarnessTrigger{URL: harness, Token: "tok", Client: http.DefaultClient}).Trigger,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		settleUnit: 50 * time.Millisecond,
		retryDelay: 20 * time.Millisecond,
	}
	srv := httptest.NewServer(rc.Handler())
	t.Cleanup(srv.Close)
	return rc, func(body string) (int, map[string]string) {
		req, _ := http.NewRequest("POST", srv.URL+"/h/"+h.Agent+"/"+h.Name, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
}

func TestReceiverObjectBatch(t *testing.T) {
	harness, triggers := objectHarness(t, 1)
	h := Hook{Agent: "a1", Name: "sessions", Enabled: true, Auth: AuthBearer, Events: []string{"object_created"},
		Prompt: `{{if hasPrefix .object.key "gh-pr-agent/"}}Analyse {{.object.key}}{{end}}`}
	_, post := objectReceiver(t, h, harness.URL)
	batch := tigrisBatch(
		tigrisEvent("OBJECT_CREATED_PUT", "gh-pr-agent/1.trace.json", "e1"),
		tigrisEvent("OBJECT_CREATED_PUT", "other-agent/2.trace.json", "e2"),
		tigrisEvent("OBJECT_DELETED", "gh-pr-agent/3.trace.json", "e3"),
		tigrisEvent("OBJECT_CREATED_MULTIPART", "gh-pr-agent/4.trace.json", "e4"),
	)
	// The first trigger finds the agent busy: the batch fails, so Tigris
	// sends all of it again. Only the event that hasn't run yet runs.
	code, out := post(batch)
	if code != http.StatusServiceUnavailable || out["started"] != "1" || out["ignored"] != "2" {
		t.Fatalf("first: %d %v", code, out)
	}
	code, out = post(batch)
	if code != http.StatusAccepted || out["started"] != "1" || out["duplicate"] != "1" || out["ignored"] != "2" {
		t.Fatalf("retry: %d %v", code, out)
	}
	code, out = post(batch)
	if code != http.StatusOK || out["duplicate"] != "2" {
		t.Fatalf("redelivery: %d %v", code, out)
	}
	got := triggers()
	if len(got) != 2 || got[0].Prompt != "Analyse gh-pr-agent/4.trace.json" || got[1].Prompt != "Analyse gh-pr-agent/1.trace.json" || got[0].Event != "object_created" {
		t.Fatalf("triggers %+v", got)
	}
	if code, _ := post(`{"events":[{"eventSource":"tigris","eventName":"OBJECT_CREATED_PUT","bucket":"b","object":{}}]}`); code != http.StatusBadRequest {
		t.Errorf("malformed batch: %d", code)
	}
}

func TestReceiverSettle(t *testing.T) {
	harness, triggers := objectHarness(t, 1)
	h := Hook{Agent: "a1", Name: "sessions", Enabled: true, Auth: AuthBearer, SettleSeconds: 2, // 100ms in this test
		Prompt: `Analyse {{.object.key}} at {{.object.etag}}`}
	rc, post := objectReceiver(t, h, harness.URL)
	// One session rewritten after each of three turns, and another session.
	for i, etag := range []string{"e1", "e2", "e3"} {
		code, out := post(tigrisBatch(tigrisEvent("OBJECT_CREATED_PUT", "a/1.trace.json", etag)))
		if code != http.StatusAccepted || out["scheduled"] != "1" {
			t.Fatalf("turn %d: %d %v", i, code, out)
		}
		time.Sleep(30 * time.Millisecond)
	}
	post(tigrisBatch(tigrisEvent("OBJECT_CREATED_PUT", "a/2.trace.json", "f1")))
	if n := len(triggers()); n != 0 || rc.Pending() != 2 {
		t.Fatalf("ran before settling: %d triggers, %d pending", n, rc.Pending())
	}
	// The first trigger finds the agent busy and is held again.
	deadline := time.Now().Add(5 * time.Second)
	for len(triggers()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := triggers()
	prompts := []string{}
	for _, r := range got {
		prompts = append(prompts, r.Prompt)
	}
	slices.Sort(prompts)
	if !slices.Equal(prompts, []string{"Analyse a/1.trace.json at e3", "Analyse a/2.trace.json at f1"}) || rc.Pending() != 0 {
		t.Fatalf("prompts %q, %d pending", prompts, rc.Pending())
	}
	// A redelivered event doesn't schedule again.
	if code, out := post(tigrisBatch(tigrisEvent("OBJECT_CREATED_PUT", "a/1.trace.json", "e3"))); code != http.StatusOK || out["duplicate"] != "1" {
		t.Errorf("redelivery: %d %v", code, out)
	}
}
