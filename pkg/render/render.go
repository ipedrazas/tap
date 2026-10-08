// Package render produces the Kubernetes manifests for one agent. The output
// contains no secret values, so it can be committed to a GitOps repo.
package render

import (
	"bytes"
	"embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/spec"
)

//go:embed templates/*.tmpl
var templates embed.FS

var tmpl = template.Must(template.ParseFS(templates, "templates/*.tmpl"))

var (
	digestRef = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9./_-]+@sha256:[a-f0-9]{64}$`)
	labelSafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

type Input struct {
	Bundle   *spec.Bundle
	Platform *spec.Platform
	// BundleRef is the pushed bundle, by digest.
	BundleRef string
}

type data struct {
	Agent                   *spec.Agent
	Platform                *spec.Platform
	Namespace, Host, Owner  string
	BundleRef, SpecHash     string
	HarnessImage            string
	RunnerCPU, RunnerMemory string
	WorkspaceSize           string
	HasSecrets              bool
	HasEgress               bool
	JobName                 string
}

// Render produces the agent's runtime manifests.
func Render(in Input) ([]byte, error) {
	d, err := prepare(in, true)
	if err != nil {
		return nil, err
	}
	return execute("agent.yaml.tmpl", d)
}

// TestNamespace holds fixture Jobs for every agent.
const TestNamespace = "tap-ci"

// RenderTest produces a Job that runs the bundle's fixtures in its runner image.
func RenderTest(in Input) ([]byte, error) {
	d, err := prepare(in, false)
	if err != nil {
		return nil, err
	}
	d.Namespace = TestNamespace
	_, digest, _ := strings.Cut(in.BundleRef, "@sha256:")
	d.JobName = "test-" + in.Bundle.Agent.Metadata.Name + "-" + digest[:10]
	return execute("test.yaml.tmpl", d)
}

func execute(name string, d *data) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func prepare(in Input, needHarness bool) (*data, error) {
	a, p := in.Bundle.Agent, in.Platform
	if !digestRef.MatchString(in.BundleRef) {
		return nil, fmt.Errorf("bundle ref %q must be pinned by digest", in.BundleRef)
	}
	if needHarness && !digestRef.MatchString(p.Harness.Image) {
		return nil, fmt.Errorf("platform.yaml harness.image %q must be pinned by digest (run `task images:push`)", p.Harness.Image)
	}
	d := &data{
		Agent:         a,
		Platform:      p,
		Namespace:     p.NamespacePrefix + a.Metadata.Name,
		Host:          a.Metadata.Name + "." + p.Domain,
		Owner:         labelSafe.ReplaceAllString(a.Metadata.Owner, "-"),
		BundleRef:     in.BundleRef,
		SpecHash:      in.Bundle.SpecHash(),
		HarnessImage:  p.Harness.Image,
		RunnerCPU:     or(a.Runner.Resources.CPU, "250m"),
		RunnerMemory:  or(a.Runner.Resources.Memory, "256Mi"),
		WorkspaceSize: or(a.Workspace.Size, "1Gi"),
		HasSecrets:    len(a.Secrets) > 0,
		HasEgress:     len(egress.PolicyFor(a)) > 0,
	}
	if d.HasEgress && p.EgressProxy.Address == "" {
		return nil, fmt.Errorf("agent declares egress but platform.yaml has no egressProxy.address")
	}
	if needHarness && p.ToolUIDBase == 0 {
		return nil, fmt.Errorf("platform.yaml toolUIDBase must be set")
	}
	return d, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
