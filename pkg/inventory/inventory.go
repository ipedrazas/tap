// Package inventory lists the agents running in the cluster. tapctl (through
// a kubeconfig) and the console (in-cluster) share it, so both show the same
// thing. Everything comes from the agent Deployments' labels and annotations
// written by `tapctl render`, and from their pods.
package inventory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	LabelAgent  = "tap.hiddenfield.dev/agent"
	annotations = "tap.hiddenfield.dev/"
)

type Agent struct {
	Name        string    `json:"name"`
	Namespace   string    `json:"namespace"`
	Version     string    `json:"version"`
	Owner       string    `json:"owner"`
	Description string    `json:"description"`
	Model       string    `json:"model"`
	Tools       []string  `json:"tools"`
	MCP         []string  `json:"mcpServers"`
	Egress      []string  `json:"egress"`
	URL         string    `json:"url"`
	Bundle      string    `json:"bundle"`
	Runner      string    `json:"runnerImage"`
	Harness     string    `json:"harnessImage"`
	Status      string    `json:"status"` // Ready, Progressing, Degraded, Scaled down
	Ready       int32     `json:"ready"`
	Desired     int32     `json:"desired"`
	Restarts    int32     `json:"restarts"`
	Created     time.Time `json:"created"`
	Pods        []Pod     `json:"pods"`
}

type Pod struct {
	Name     string    `json:"name"`
	Phase    string    `json:"phase"`
	Ready    bool      `json:"ready"`
	Restarts int32     `json:"restarts"`
	Node     string    `json:"node"`
	Started  time.Time `json:"started"`
	// Reason explains a container that is waiting or crashed (e.g. ImagePullBackOff).
	Reason string `json:"reason,omitempty"`
}

// ScriptTools and MCPTools split Tools by kind.
func (a Agent) ScriptTools() int {
	n := 0
	for _, t := range a.Tools {
		if !strings.Contains(t, "__") {
			n++
		}
	}
	return n
}

func (a Agent) MCPTools() int { return len(a.Tools) - a.ScriptTools() }

// List returns every agent, sorted by name.
func List(ctx context.Context, c kubernetes.Interface) ([]Agent, error) {
	sel := metav1.ListOptions{LabelSelector: LabelAgent}
	deps, err := c.AppsV1().Deployments("").List(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	pods, err := c.CoreV1().Pods("").List(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	byAgent := map[string][]corev1.Pod{}
	for _, p := range pods.Items {
		key := p.Namespace + "/" + p.Labels[LabelAgent]
		byAgent[key] = append(byAgent[key], p)
	}
	var out []Agent
	for _, d := range deps.Items {
		out = append(out, fromDeployment(d, byAgent[d.Namespace+"/"+d.Labels[LabelAgent]]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one agent by name.
func Get(ctx context.Context, c kubernetes.Interface, name string) (*Agent, error) {
	all, err := List(ctx, c)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("no agent named %q is running", name)
}

func fromDeployment(d appsv1.Deployment, pods []corev1.Pod) Agent {
	ann := func(k string) string { return d.Annotations[annotations+k] }
	a := Agent{
		Name:        d.Labels[LabelAgent],
		Namespace:   d.Namespace,
		Version:     d.Labels["app.kubernetes.io/version"],
		Owner:       ann("owner"),
		Description: ann("description"),
		Model:       ann("model"),
		Tools:       split(ann("tools")),
		MCP:         split(ann("mcp")),
		Egress:      split(ann("egress")),
		URL:         ann("url"),
		Bundle:      d.Spec.Template.Annotations[annotations+"bundle"],
		Ready:       d.Status.ReadyReplicas,
		Created:     d.CreationTimestamp.Time,
	}
	if d.Spec.Replicas != nil {
		a.Desired = *d.Spec.Replicas
	}
	for _, c := range d.Spec.Template.Spec.InitContainers {
		if c.Name == "runner" {
			a.Runner = c.Image
		}
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "harness" {
			a.Harness = c.Image
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].CreationTimestamp.After(pods[j].CreationTimestamp.Time) })
	for _, p := range pods {
		pod := Pod{Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName}
		if p.Status.StartTime != nil {
			pod.Started = p.Status.StartTime.Time
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady {
				pod.Ready = cond.Status == corev1.ConditionTrue
			}
		}
		statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
		for _, cs := range statuses {
			pod.Restarts += cs.RestartCount
			if w := cs.State.Waiting; w != nil && pod.Reason == "" {
				pod.Reason = cs.Name + ": " + w.Reason
			}
			if t := cs.LastTerminationState.Terminated; t != nil && pod.Reason == "" && !pod.Ready {
				pod.Reason = cs.Name + ": " + t.Reason
			}
		}
		a.Restarts += pod.Restarts
		a.Pods = append(a.Pods, pod)
	}
	a.Status = status(d, a)
	return a
}

func status(d appsv1.Deployment, a Agent) string {
	switch {
	case a.Desired == 0:
		return "Scaled down"
	case a.Ready >= a.Desired && d.Status.UpdatedReplicas >= a.Desired && d.Status.Replicas == a.Desired:
		return "Ready"
	case a.Ready == 0:
		for _, p := range a.Pods {
			if p.Reason != "" {
				return "Degraded"
			}
		}
		return "Progressing"
	default:
		return "Progressing"
	}
}

func split(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

// Age formats a duration like kubectl (5m, 3h, 2d).
func Age(t time.Time, now time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ShortDigest shortens image@sha256:... to its first 12 hex digits.
func ShortDigest(ref string) string {
	_, d, ok := strings.Cut(ref, "@sha256:")
	if !ok || len(d) < 12 {
		return ref
	}
	return d[:12]
}
