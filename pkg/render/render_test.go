package render

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ipedrazas/tap/pkg/spec"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	harnessRef = "registry.hiddenfield.dev/tap/harness@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bundleRef  = "registry.hiddenfield.dev/agents/echo-agent@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func input(t *testing.T) Input { return inputFor(t, "echo-agent") }

func inputFor(t *testing.T, agent string) Input {
	t.Helper()
	b, err := spec.Load("../../agents/" + agent)
	if err != nil {
		t.Fatal(err)
	}
	p, err := spec.LoadPlatform("../../platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p.Harness.Image = harnessRef
	return Input{Bundle: b, Platform: p, BundleRef: bundleRef}
}

func TestGolden(t *testing.T) {
	// echo-agent: no secrets, no egress. weather-agent: egress through the proxy.
	// countries-agent: secrets from OpenBao.
	for _, agent := range []string{"echo-agent", "weather-agent", "countries-agent"} {
		t.Run(agent, func(t *testing.T) { golden(t, agent) })
	}
}

func golden(t *testing.T, agent string) {
	out, err := Render(inputFor(t, agent))
	if err != nil {
		t.Fatal(err)
	}
	// Every document must be valid YAML.
	for i, doc := range strings.Split(string(out), "\n---\n") {
		var v map[string]any
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("document %d: %v", i, err)
		}
		if v["kind"] == nil {
			t.Fatalf("document %d has no kind", i)
		}
	}
	golden := "testdata/" + agent + ".yaml"
	if *update {
		if err := os.WriteFile(golden, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("render output differs from %s; run go test ./pkg/render -update and review the diff", golden)
	}
}

func TestRequiresDigests(t *testing.T) {
	in := input(t)
	in.BundleRef = "registry.hiddenfield.dev/agents/echo-agent:0.1.0"
	if _, err := Render(in); err == nil {
		t.Fatal("expected error for tag reference")
	}
	in = input(t)
	in.Platform.Harness.Image = ""
	if _, err := Render(in); err == nil {
		t.Fatal("expected error for unpinned harness")
	}
}

func TestSessionsOff(t *testing.T) {
	in := input(t)
	in.Platform.Sessions = spec.Sessions{}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	// echo-agent has no tool egress, so without session export it gets
	// neither the proxy route nor any key for it.
	for _, s := range []string{"--sessions-", "sessions-credentials", "egress-key", "egress-proxy"} {
		if strings.Contains(string(out), s) {
			t.Errorf("render with sessions off contains %q", s)
		}
	}
	in.Platform.Sessions = spec.Sessions{Endpoint: "http://t3.storage.dev", Bucket: "b"}
	if _, err := Render(in); err == nil {
		t.Error("expected error for a non-https sessions endpoint")
	}
}

func TestAdmission(t *testing.T) {
	p, err := spec.LoadPlatform("../../platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[string][]string{
		"audit":   {"validationActions: [Audit]", "failurePolicy: Ignore"},
		"enforce": {"validationActions: [Deny]", "failurePolicy: Fail"},
	} {
		p.Signing.Admission = mode
		out, err := Admission(p)
		if err != nil {
			t.Fatal(err)
		}
		docs := strings.Split(string(out), "\n---\n")
		if len(docs) != 6 {
			t.Fatalf("%s: want 2 Kyverno policies and 2 native policies with bindings, got %d documents", mode, len(docs))
		}
		for i, doc := range docs[2:] {
			var v map[string]any
			if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
				t.Fatalf("%s native doc %d: %v", mode, i, err)
			}
		}
		if mode == "enforce" && !strings.Contains(docs[3], "validationActions: [Deny]") {
			t.Fatalf("enforce: binding does not deny:\n%s", docs[3])
		}
		if strings.Contains(string(out), "<no value>") {
			t.Fatalf("%s: a template field rendered empty", mode)
		}
		for i, doc := range docs[:2] {
			var v struct {
				Kind string `json:"kind"`
				Spec struct {
					Attestors []struct {
						Cosign struct {
							Key struct{ Data string } `json:"key"`
						} `json:"cosign"`
					} `json:"attestors"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
				t.Fatalf("%s doc %d: %v", mode, i, err)
			}
			if v.Kind != "ImageValidatingPolicy" || len(v.Spec.Attestors) != 2-i ||
				strings.TrimSpace(v.Spec.Attestors[0].Cosign.Key.Data) != strings.TrimSpace(p.Signing.PublicKey) {
				t.Fatalf("%s doc %d: kind %q or key not carried through", mode, i, v.Kind)
			}
			for _, w := range want {
				if !strings.Contains(doc, w) {
					t.Fatalf("%s doc %d lacks %q", mode, i, w)
				}
			}
		}
		if !strings.Contains(docs[0], strings.Split(strings.TrimSpace(p.Signing.TestedPublicKey), "\n")[1]) || !strings.Contains(docs[0], "attestors.tested") {
			t.Fatalf("%s: tap-agents does not require the tested signature", mode)
		}
	}
	p.Signing.Admission = "warn"
	if _, err := Admission(p); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
	p.Signing.Admission, p.Signing.TestedPublicKey = "audit", p.Signing.PublicKey
	if _, err := Admission(p); err == nil {
		t.Fatal("expected an error when the tested key is the build key")
	}
}
