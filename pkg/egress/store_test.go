package egress

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	newKey    = strings.Repeat("n", 64)
	legacyKey = strings.Repeat("l", 64)
)

// fakeKube serves one labelled policy ConfigMap (echo-agent), the legacy
// ConfigMap (echo-agent and old-agent) and the legacy keys Secret.
func fakeKube(t *testing.T) *APIStore { return fakeKubeDown(t, &atomic.Bool{}) }

// fakeKubeDown is fakeKube, answering 503 while down is set.
func fakeKubeDown(t *testing.T, down *atomic.Bool) *APIStore {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sa" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/namespaces/tap-system/configmaps" && r.URL.Query().Get("labelSelector") == PolicyLabel:
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{
				"metadata": map[string]any{"labels": map[string]string{PolicyLabel: "echo-agent"}},
				"data":     map[string]string{"policy.json": `{"new":["new.example:443"]}`},
			}}})
		case r.URL.Path == "/api/v1/namespaces/tap-system/configmaps/egress-policy":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
				"echo-agent.json": `{"old":["old.example:443"]}`,
				"old-agent.json":  `{"old":["old.example:443"]}`,
			}})
		case r.URL.Path == "/api/v1/namespaces/tap-system/secrets/egress-keys":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
				"echo-agent": base64.StdEncoding.EncodeToString([]byte(legacyKey)),
				"old-agent":  base64.StdEncoding.EncodeToString([]byte(legacyKey + "\n")),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &APIStore{
		Namespace: "tap-system", SecretName: "egress-keys", ConfigMapName: "egress-policy",
		MaxAge: time.Minute, client: srv.Client(), host: srv.URL,
		token: func() (string, error) { return "sa", nil },
	}
}

type fakeBao struct {
	srv          *httptest.Server
	logins, gets atomic.Int32
	requests     atomic.Int32
	down         atomic.Bool
	revoke       atomic.Bool
}

// newFakeBao has a key for agent-echo-agent only.
func newFakeBao(t *testing.T) (*fakeBao, *OpenBao) {
	t.Helper()
	f := &fakeBao{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/v1/auth/kubernetes-tap/login":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["role"] != "tap-egress-proxy" || body["jwt"] != "projected" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.logins.Add(1)
			f.revoke.Store(false)
			json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "bao", "lease_duration": 600}})
		case "/v1/secret/data/tap-egress/agent-echo-agent", "/v1/secret/data/tap-egress/agent-other-agent":
			f.gets.Add(1)
			if r.Header.Get("X-Vault-Token") != "bao" || f.revoke.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if strings.HasSuffix(r.URL.Path, "other-agent") {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"key": newKey}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("projected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, &OpenBao{
		Addr: f.srv.URL, AuthMount: "kubernetes-tap", Role: "tap-egress-proxy", KVMount: "secret",
		Prefix: "tap-egress", NamespacePrefix: "agent-", TokenFile: tok, MaxAge: time.Minute,
	}
}

func TestAPIStorePolicies(t *testing.T) {
	s := fakeKube(t)
	p, err := s.Policy("echo-agent")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Allows("new", "new.example:443") || p.Allows("old", "old.example:443") {
		t.Errorf("labelled ConfigMap should win over the legacy one: %v", p)
	}
	if p, err := s.Policy("old-agent"); err != nil || !p.Allows("old", "old.example:443") {
		t.Errorf("legacy policy: %v %v", p, err)
	}
	if _, err := s.Policy("missing-agent"); err == nil {
		t.Error("missing agent has a policy")
	}
}

func TestAPIStoreKeys(t *testing.T) {
	s := fakeKube(t)
	f, bao := newFakeBao(t)
	s.Keys = bao

	if k, err := s.Key("echo-agent"); err != nil || string(k) != newKey {
		t.Fatalf("OpenBao key should win: %q %v", k, err)
	}
	if k, err := s.Key("old-agent"); err != nil || string(k) != legacyKey {
		t.Fatalf("legacy fallback: %q %v", k, err)
	}
	if _, err := s.Key("missing-agent"); err == nil {
		t.Fatal("missing agent has a key")
	}

	// Cached: no new reads within MaxAge.
	gets := f.gets.Load()
	s.Key("echo-agent")
	if f.gets.Load() != gets {
		t.Error("cached key was refetched")
	}

	// A revoked token logs in again once.
	bao.keys = nil
	f.revoke.Store(true)
	logins := f.logins.Load()
	if k, err := s.Key("echo-agent"); err != nil || string(k) != newKey {
		t.Fatalf("after revocation: %q %v", k, err)
	}
	if f.logins.Load() != logins+1 {
		t.Errorf("logins: got %d, want %d", f.logins.Load(), logins+1)
	}

	// OpenBao down: a cached key keeps working, an unknown one fails.
	bao.MaxAge = 0
	f.down.Store(true)
	time.Sleep(1100 * time.Millisecond)
	if k, err := s.Key("echo-agent"); err != nil || string(k) != newKey {
		t.Errorf("stale key during outage: %q %v", k, err)
	}
	if _, err := s.Key("other-agent"); err == nil {
		t.Error("uncached key served during outage")
	}
	if k, err := s.Key("old-agent"); err != nil || string(k) != legacyKey {
		t.Errorf("legacy key during outage: %q %v", k, err)
	}
}

// An API server outage doesn't stop egress: the last good policies keep
// serving, up to MaxStale.
func TestAPIStoreServesLastGoodPolicy(t *testing.T) {
	var down atomic.Bool
	s := fakeKubeDown(t, &down)
	s.MaxAge = 0
	if _, err := s.Policy("echo-agent"); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	for range 3 {
		if p, err := s.Policy("echo-agent"); err != nil || !p.Allows("new", "new.example:443") {
			t.Fatalf("during the outage: %v %v", p, err)
		}
	}
	s.MaxStale = time.Nanosecond
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.Policy("echo-agent"); err == nil {
		t.Fatal("served a policy older than MaxStale")
	}
}

// A failing OpenBao read is cached for a second: a burst of calls for an
// agent makes one request, not one each.
func TestOpenBaoCachesErrors(t *testing.T) {
	f, bao := newFakeBao(t)
	f.down.Store(true)
	before := f.requests.Load()
	for range 5 {
		if _, err := bao.Key("echo-agent"); err == nil {
			t.Fatal("no error while OpenBao is down")
		}
	}
	if n := f.requests.Load() - before; n != 1 {
		t.Fatalf("%d requests to OpenBao, want 1", n)
	}
}
