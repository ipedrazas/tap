// Package render produces the Kubernetes manifests for one agent. The output
// contains no secret values, so it can be committed to a GitOps repo.
package render

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/spec"
)

//go:embed templates/*.tmpl
var templates embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	// quote emits a JSON string, which is always a valid YAML scalar.
	"quote": func(s string) string { b, _ := json.Marshal(s); return string(b) },
	"indent": func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		return pad + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pad)
	},
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
}).ParseFS(templates, "templates/*.tmpl"))

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
	// Comma-separated summaries for the inventory annotations.
	ToolNames, MCPNames, EgressHosts, URL string
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
	var tools, servers, hosts []string
	for _, t := range a.Tools {
		tools = append(tools, t.Name)
	}
	for _, s := range a.MCP {
		servers = append(servers, s.Name)
		for _, t := range s.Tools {
			tools = append(tools, spec.MCPToolName(s.Name, t.Name))
		}
	}
	for _, list := range egress.PolicyFor(a) {
		for _, h := range list {
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	slices.Sort(hosts)
	d.ToolNames, d.MCPNames, d.EgressHosts = strings.Join(tools, ","), strings.Join(servers, ","), strings.Join(hosts, ",")
	d.URL = "https://" + d.Host
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

// Admission renders the Kyverno policies that verify agent and fixture pods
// against platform.yaml's signing key, in its admission mode.
func Admission(p *spec.Platform) ([]byte, error) {
	s := p.Signing
	for name, key := range map[string]string{"publicKey": s.PublicKey, "testedPublicKey": s.TestedPublicKey} {
		if !strings.Contains(key, "BEGIN PUBLIC KEY") {
			return nil, fmt.Errorf("platform.yaml signing.%s is not a PEM public key (run `task signing:key`)", name)
		}
	}
	if s.PublicKey == s.TestedPublicKey {
		return nil, fmt.Errorf("platform.yaml signing.testedPublicKey must differ from publicKey")
	}
	d := map[string]any{
		"Mode":            s.Admission,
		"Registry":        p.Registry,
		"PublicKey":       s.PublicKey,
		"TestedPublicKey": s.TestedPublicKey,
	}
	type rule struct{ Group, Resources string }
	d["RegistryRE"] = regexp.QuoteMeta(p.Registry)
	d["RegistryKinds"] = []struct {
		Name, Spec string
		Rules      []rule
	}{
		// ephemeralcontainers too: kubectl debug must not bring in foreign images.
		{"pods", "object.spec", []rule{{"", "pods, pods/ephemeralcontainers"}}},
		{"workloads", "object.spec.template.spec", []rule{{"apps", "deployments"}, {"batch", "jobs"}}},
	}
	switch s.Admission {
	case "audit":
		// Violations go to PolicyReports; a Kyverno outage blocks nothing.
		d["Action"], d["FailurePolicy"], d["VAPActions"], d["Timeout"] = "Audit", "Ignore", "Warn, Audit", 5
	case "enforce":
		d["Action"], d["FailurePolicy"], d["VAPActions"], d["Timeout"] = "Deny", "Fail", "Deny", 25
	default:
		return nil, fmt.Errorf("platform.yaml signing.admission must be audit or enforce, not %q", s.Admission)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "admission.yaml.tmpl", d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
