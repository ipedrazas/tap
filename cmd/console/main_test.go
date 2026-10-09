package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ipedrazas/tap/pkg/inventory"
)

func TestConsole(t *testing.T) {
	one := int32(1)
	c := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "agent-demo-agent",
			Labels:      map[string]string{inventory.LabelAgent: "demo-agent", "app.kubernetes.io/version": "1.2.3"},
			Annotations: map[string]string{"tap.hiddenfield.dev/description": "<script>alert(1)</script>", "tap.hiddenfield.dev/tools": "a,b"}},
		Spec:   appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
	})
	srv := httptest.NewServer(handler(c, slog.New(slog.NewTextHandler(io.Discard, nil)), nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)
	for _, want := range []string{"demo-agent", "1.2.3", `class="status ready"`, "&lt;script&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(html, "<script>alert") {
		t.Error("description must be escaped")
	}

	resp, err = http.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	var agents []inventory.Agent
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil || len(agents) != 1 || len(agents[0].Tools) != 2 {
		t.Fatalf("api: %v %+v", err, agents)
	}
	resp.Body.Close()
	if resp, _ := http.Get(srv.URL + "/api/agents/missing"); resp.StatusCode != 404 {
		t.Fatalf("missing agent: %d", resp.StatusCode)
	}
}

// idToken builds an unsigned JWT; the gateway verifies the real one.
func idToken(email string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(`{"email":"`+email+`"}`) + ".sig"
}

func TestFactoryProxy(t *testing.T) {
	var got *http.Request
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer factory.Close()
	u, _ := url.Parse(factory.URL)
	srv := httptest.NewServer(handler(fake.NewSimpleClientset(), slog.New(slog.NewTextHandler(io.Discard, nil)), u))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/factory/jobs", nil)
	req.Header.Set("x-tap-identity", idToken("alice@example.com"))
	req.Header.Set("x-tap-user", "mallory@example.com")
	req.Header.Set("Cookie", "tap-access=secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("proxy: %v %v", err, resp)
	}
	resp.Body.Close()
	if got.URL.Path != "/v1/jobs" || got.Header.Get("x-tap-user") != "alice@example.com" {
		t.Errorf("forwarded %s as %q", got.URL.Path, got.Header.Get("x-tap-user"))
	}
	if got.Header.Get("x-tap-identity") != "" || got.Header.Get("Cookie") != "" {
		t.Error("ID token and cookies must not reach the factory")
	}

	// Not signed in.
	if resp, _ := http.Get(srv.URL + "/api/factory/jobs"); resp.StatusCode != 401 {
		t.Errorf("anonymous: %d", resp.StatusCode)
	}
	// A cross-site form post (not JSON) is refused before it reaches the factory.
	got = nil
	req, _ = http.NewRequest("POST", srv.URL+"/api/factory/jobs", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-tap-identity", idToken("alice@example.com"))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 415 || got != nil {
		t.Errorf("form post: %d forwarded=%v", resp.StatusCode, got != nil)
	}
	if resp, _ := http.Get(srv.URL + "/factory"); resp.StatusCode != 200 {
		t.Errorf("factory page: %d", resp.StatusCode)
	}
}
