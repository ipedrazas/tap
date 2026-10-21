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
	"strings"
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
