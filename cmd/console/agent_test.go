package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ipedrazas/tap/pkg/hooks"
	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/spec"
)

const demoYAML = `apiVersion: tavon.ai/agent/v1
kind: Agent
metadata:
  name: demo-agent
  version: 0.1.2
  owner: platform
  title: Demo
  description: Demo agent
harness:
  api: v1
  model: { provider: gateway, name: agent }
effectsPolicy: { write: deny, irreversible: deny }
prompt: system.md
runner:
  image: registry.example/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000
tools:
  - name: lookup
    description: Look something up
    input_schema: { type: object, properties: {}, additionalProperties: false }
    exec: ["python3", "tools/lookup.py"]
    egress: [api.example.com:443]
    effects: read
`

type settingsEnv struct {
	srv     *httptest.Server
	k8s     *fake.Clientset
	dyn     *dynamicfake.FakeDynamicClient
	mu      sync.Mutex
	puts    map[string]string // OpenBao path -> value
	factory []*http.Request
	bodies  []string
	runs    *fakeRunStore
	// platform is the platform.yaml the console reads.
	platform string
}

func externalSecret(ns string, refs map[string]string) *unstructured.Unstructured {
	var data []any
	for name, key := range refs {
		data = append(data, map[string]any{"secretKey": name, "remoteRef": map[string]any{"key": key, "property": "value"}})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
		"metadata": map[string]any{"name": "tool-secrets", "namespace": ns},
		"spec":     map[string]any{"data": data},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}
}

func newSettingsEnv(t *testing.T, refs map[string]string) *settingsEnv {
	t.Helper()
	e := &settingsEnv{puts: map[string]string{}, runs: &fakeRunStore{objects: map[string]fakeObject{}}}
	one := int32(1)
	e.k8s = fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "agent-demo-agent",
				Labels:      map[string]string{inventory.LabelAgent: "demo-agent", "app.kubernetes.io/version": "0.1.2"},
				Annotations: map[string]string{"tap.hiddenfield.dev/owner": "platform", "tap.hiddenfield.dev/title": "Demo"}},
			Spec:   appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
		},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settingsConfigMap, Namespace: "tap-system"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hooks.Secret, Namespace: "tap-system"}},
	)
	e.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{externalSecrets: "ExternalSecretList"},
		externalSecret("agent-demo-agent", refs))

	bao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/auth/kubernetes-tap/login":
			io.WriteString(w, `{"auth":{"client_token":"t","lease_duration":600}}`)
		case r.Header.Get("X-Vault-Token") != "t":
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/secret/metadata/"):
			e.mu.Lock()
			_, ok := e.puts[strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/")]
			e.mu.Unlock()
			if !ok {
				http.Error(w, `{"errors":[]}`, 404)
				return
			}
			io.WriteString(w, `{"data":{"updated_time":"2026-10-12T10:00:00Z"}}`)
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/secret/data/"):
			var body struct {
				Data map[string]string `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			e.mu.Lock()
			e.puts[strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")] = body.Data["value"]
			e.mu.Unlock()
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(bao.Close)

	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.factory = append(e.factory, r.Clone(context.Background()))
		e.bodies = append(e.bodies, string(b))
		e.mu.Unlock()
		switch r.URL.Path {
		case "/console/agents/demo-agent":
			json.NewEncoder(w).Encode(AgentSpec{SHA: strings.Repeat("a", 40), AgentYAML: demoYAML})
		case "/console/agents/demo-agent/pr":
			io.WriteString(w, `{"number":7,"html_url":"https://github.com/o/r/pull/7"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(factory.Close)

	dir := t.TempDir()
	platform := filepath.Join(dir, "platform.yaml")
	os.WriteFile(platform, []byte("namespacePrefix: agent-\nsecrets: {server: "+bao.URL+", kvMount: secret, prefix: tap, authMount: kubernetes-tap}\nhooks: {publicURL: https://hooks.example.com}\nteams:\n  platform: [Alice@example.com]\n"), 0o600)
	jwt := filepath.Join(dir, "token")
	os.WriteFile(jwt, []byte("jwt\n"), 0o600)
	fu, _ := url.Parse(factory.URL)
	d := deps{
		K8s: e.k8s, Dyn: e.dyn, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Factory: fu, FactoryToken: func() string { return "tok" },
		Platform: &platformFile{path: platform}, Titles: &titles{c: e.k8s, namespace: "tap-system"}, Hooks: &hooks.Store{C: e.k8s, Namespace: "tap-system"},
		Bao:  &openBao{cfg: spec.Secrets{Server: bao.URL, KVMount: "secret", AuthMount: "kubernetes-tap"}, role: "tap-console", jwtFile: jwt, http: http.DefaultClient},
		Runs: newRuns(e.runs),
	}
	e.platform = platform
	e.srv = httptest.NewServer(handler(d))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *settingsEnv) do(t *testing.T, method, path, email, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if email != "" {
		req.Header.Set("x-tap-identity", idToken(email))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSettingsPage(t *testing.T) {
	e := newSettingsEnv(t, map[string]string{"API_KEY": "tap/agent-demo-agent/api-key"})
	code, html := e.do(t, "GET", "/agents/demo-agent", "alice@example.com", "")
	if code != 200 {
		t.Fatalf("page: %d %s", code, html)
	}
	for _, want := range []string{"<h1>Demo</h1>", "API_KEY", "missing", "api.example.com:443", `data-tool="lookup"`, "version 0.1.2 on main"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(html, "Only members") {
		t.Error("alice is in team platform")
	}
	if _, html := e.do(t, "GET", "/agents/demo-agent", "bob@example.com", ""); !strings.Contains(html, "Only members of team platform") {
		t.Error("bob should see the page read-only")
	}
}

func TestSettingsKeepPlatformOnBadReload(t *testing.T) {
	e := newSettingsEnv(t, nil)
	if _, html := e.do(t, "GET", "/agents/demo-agent", "alice@example.com", ""); strings.Contains(html, "failed to reload") {
		t.Fatal("note before any bad reload")
	}
	// A field this build does not know, as when platform.yaml is newer than the console.
	os.WriteFile(e.platform, []byte("teams:\n  platform: [alice@example.com]\nsecrets: {newField: x}\n"), 0o600)
	later := time.Now().Add(time.Minute)
	os.Chtimes(e.platform, later, later)
	_, html := e.do(t, "GET", "/agents/demo-agent", "alice@example.com", "")
	if !strings.Contains(html, "failed to reload") || !strings.Contains(html, "newField") {
		t.Errorf("page should name the reload error:\n%s", html)
	}
	if strings.Contains(html, "read-only") {
		t.Error("alice lost edit rights to a bad reload")
	}
	if code, body := e.do(t, "PUT", "/api/agents/demo-agent/title", "alice@example.com", `{"title":"Weather"}`); code != 200 {
		t.Errorf("set title on the last good platform.yaml: %d %s", code, body)
	}
}

func TestSettingsTitle(t *testing.T) {
	e := newSettingsEnv(t, nil)
	if code, _ := e.do(t, "PUT", "/api/agents/demo-agent/title", "bob@example.com", `{"title":"x"}`); code != 403 {
		t.Errorf("non-member: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/agents/demo-agent/title", "", `{"title":"x"}`); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/agents/demo-agent/title", "alice@example.com", `{"title":"a\nb"}`); code != 422 {
		t.Errorf("multi-line title: %d", code)
	}
	if code, body := e.do(t, "PUT", "/api/agents/demo-agent/title", "alice@example.com", `{"title":"  Weather  "}`); code != 200 {
		t.Fatalf("set: %d %s", code, body)
	}
	cm, _ := e.k8s.CoreV1().ConfigMaps("tap-system").Get(context.Background(), settingsConfigMap, metav1.GetOptions{})
	if cm.Data["demo-agent.title"] != "Weather" {
		t.Errorf("stored %v", cm.Data)
	}
	if _, html := e.do(t, "GET", "/", "", ""); !strings.Contains(html, ">Weather</a>") || !strings.Contains(html, "demo-agent</code>") {
		t.Error("index should show the override and the name")
	}
	e.do(t, "PUT", "/api/agents/demo-agent/title", "alice@example.com", `{"title":""}`)
	cm, _ = e.k8s.CoreV1().ConfigMaps("tap-system").Get(context.Background(), settingsConfigMap, metav1.GetOptions{})
	if _, ok := cm.Data["demo-agent.title"]; ok {
		t.Error("empty title should remove the override")
	}
	// A cross-site form post is refused.
	req, _ := http.NewRequest("PUT", e.srv.URL+"/api/agents/demo-agent/title", strings.NewReader("title=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-tap-identity", idToken("alice@example.com"))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 415 {
		t.Errorf("form post: %d", resp.StatusCode)
	}
}

func TestSettingsSecret(t *testing.T) {
	e := newSettingsEnv(t, map[string]string{"API_KEY": "tap/agent-demo-agent/api-key", "STOLEN": "tap/agent-other/api-key"})
	if code, _ := e.do(t, "PUT", "/api/agents/demo-agent/secrets/API_KEY", "bob@example.com", `{"value":"v"}`); code != 403 {
		t.Errorf("non-member: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/agents/demo-agent/secrets/NOPE", "alice@example.com", `{"value":"v"}`); code != 404 {
		t.Errorf("undeclared: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/agents/demo-agent/secrets/STOLEN", "alice@example.com", `{"value":"v"}`); code != 403 {
		t.Errorf("path outside the agent's prefix: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/agents/demo-agent/secrets/API_KEY", "alice@example.com", `{"value":""}`); code != 422 {
		t.Errorf("empty: %d", code)
	}
	if code, body := e.do(t, "PUT", "/api/agents/demo-agent/secrets/API_KEY", "alice@example.com", `{"value":"s3cret"}`); code != 200 {
		t.Fatalf("set: %d %s", code, body)
	}
	if e.puts["tap/agent-demo-agent/api-key"] != "s3cret" || len(e.puts) != 1 {
		t.Errorf("openbao writes: %v", e.puts)
	}
	es, _ := e.dyn.Resource(externalSecrets).Namespace("agent-demo-agent").Get(context.Background(), "tool-secrets", metav1.GetOptions{})
	if es.GetAnnotations()["force-sync"] == "" {
		t.Error("ExternalSecret was not force-synced")
	}
	if _, html := e.do(t, "GET", "/agents/demo-agent", "alice@example.com", ""); strings.Contains(html, "s3cret") || !strings.Contains(html, `class="ok">set</span>`) {
		t.Error("page should show API_KEY as set, never its value")
	}
}

func TestSettingsPR(t *testing.T) {
	e := newSettingsEnv(t, nil)
	if code, _ := e.do(t, "POST", "/api/agents/demo-agent/pr", "bob@example.com", `{"base":"x","egress":{"lookup":["a.com:443"]}}`); code != 403 {
		t.Errorf("non-member: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/agents/demo-agent/pr", "alice@example.com", `{"base":"x","egress":{"lookup":["*.example.com:443"]}}`); code != 422 {
		t.Errorf("wildcard: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/agents/demo-agent/pr", "alice@example.com", `{"base":"x"}`); code != 422 {
		t.Errorf("no-op: %d", code)
	}
	e.factory, e.bodies = nil, nil
	code, body := e.do(t, "POST", "/api/agents/demo-agent/pr", "alice@example.com", `{"base":"abc","title":"Demo 2","egress":{"lookup":["api.example.com:443","b.example.com:443"]}}`)
	if code != 200 || !strings.Contains(body, `"number": 7`) {
		t.Fatalf("pr: %d %s", code, body)
	}
	if len(e.factory) != 1 || e.factory[0].URL.Path != "/console/agents/demo-agent/pr" || e.factory[0].Header.Get("x-tap-user") != "alice@example.com" || e.factory[0].Header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("factory calls: %v", e.factory)
	}
	var sent EditRequest
	json.Unmarshal([]byte(e.bodies[0]), &sent)
	if sent.Base != "abc" || *sent.Title != "Demo 2" || len(sent.Egress["lookup"]) != 2 {
		t.Errorf("sent %+v", sent)
	}
}

func TestCanEdit(t *testing.T) {
	p := &spec.Platform{Teams: map[string][]string{"platform": {"alice@example.com"}}}
	for _, c := range []struct {
		email, owner string
		want         bool
	}{{"alice@example.com", "platform", true}, {"ALICE@example.com", "platform", true}, {"bob@example.com", "platform", false}, {"alice@example.com", "other", false}, {"", "platform", false}} {
		if got := canEdit(p, c.email, c.owner); got != c.want {
			t.Errorf("canEdit(%q, %q) = %v", c.email, c.owner, got)
		}
	}
	if canEdit(nil, "alice@example.com", "platform") {
		t.Error("no platform.yaml: read-only")
	}
	if !ownPath(&spec.Platform{Secrets: spec.Secrets{Prefix: "tap"}}, "agent-a", "tap/agent-a/k") || ownPath(&spec.Platform{Secrets: spec.Secrets{Prefix: "tap"}}, "agent-a", "tap/agent-a/../agent-b/k") {
		t.Error("ownPath")
	}
}
