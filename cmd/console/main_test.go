package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	srv := httptest.NewServer(handler(c, slog.New(slog.NewTextHandler(io.Discard, nil))))
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
