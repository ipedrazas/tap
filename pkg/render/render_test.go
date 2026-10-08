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

func input(t *testing.T) Input {
	t.Helper()
	b, err := spec.Load("../../agents/echo-agent")
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
	out, err := Render(input(t))
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
	const golden = "testdata/echo-agent.yaml"
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
