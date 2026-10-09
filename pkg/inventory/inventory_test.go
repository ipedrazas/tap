package inventory

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func deployment(name string, ready int32) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent", Namespace: "agent-" + name,
			Labels: map[string]string{LabelAgent: name, "app.kubernetes.io/version": "0.1.0"},
			Annotations: map[string]string{
				"tap.hiddenfield.dev/model": "agent", "tap.hiddenfield.dev/tools": "lookup,wiki__ask",
				"tap.hiddenfield.dev/mcp": "wiki", "tap.hiddenfield.dev/egress": "api.example.com:443",
				"tap.hiddenfield.dev/url": "https://" + name + ".a.hiddenfield.dev",
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Hour)),
		},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"tap.hiddenfield.dev/bundle": "r/agents/" + name + "@sha256:0123456789abcdef0123"}},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "runner", Image: "runner@sha256:aa"}},
				Containers:     []corev1.Container{{Name: "harness", Image: "harness@sha256:bb"}},
			},
		}},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: ready},
	}
}

func pod(agent, reason string, restarts int32) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: agent + "-pod", Namespace: "agent-" + agent, Labels: map[string]string{LabelAgent: agent}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "harness", RestartCount: restarts}},
		},
	}
	if reason != "" {
		p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: reason}
	}
	return p
}

func TestList(t *testing.T) {
	c := fake.NewSimpleClientset(
		deployment("weather-agent", 1), pod("weather-agent", "", 0),
		deployment("broken-agent", 0), pod("broken-agent", "ImagePullBackOff", 3),
		// Not an agent: no label.
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "zot", Namespace: "tap-system"}},
	)
	agents, err := List(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].Name != "broken-agent" {
		t.Fatalf("got %+v", agents)
	}
	b, w := agents[0], agents[1]
	if b.Status != "Degraded" || b.Restarts != 3 || b.Pods[0].Reason != "harness: ImagePullBackOff" {
		t.Errorf("broken: %+v", b)
	}
	if w.Status != "Ready" || w.ScriptTools() != 1 || w.MCPTools() != 1 || w.Runner != "runner@sha256:aa" || w.URL == "" {
		t.Errorf("weather: %+v", w)
	}
	if ShortDigest(w.Bundle) != "0123456789ab" {
		t.Errorf("short digest %q", ShortDigest(w.Bundle))
	}
	if _, err := Get(context.Background(), c, "nope"); err == nil {
		t.Error("expected not found")
	}
}
