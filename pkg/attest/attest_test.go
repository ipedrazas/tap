package attest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ipedrazas/tap/pkg/runner"
	"github.com/ipedrazas/tap/pkg/spec"
)

const ref = "registry.hiddenfield.dev/agents/echo-agent@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestBuild(t *testing.T) {
	b, err := spec.Load("../../agents/echo-agent")
	if err != nil {
		t.Fatal(err)
	}
	rep := runner.Report{Passed: 3, Cases: []runner.CaseReport{{Tool: "echo_text", Case: "x", Result: "pass"}}}
	p, err := Build(Input{
		Bundle: b, Dir: "../../agents/echo-agent", BundleRef: ref, Report: rep,
		Environment: "cluster:tap-ci/test", Diff: spec.Diff(nil, b), BuilderID: "local",
		Now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// The admission policy reads these paths; keep them stable.
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	fx := v["fixtures"].(map[string]any)
	if fx["ok"] != true || fx["failed"] != float64(0) || fx["passed"] != float64(3) {
		t.Fatalf("fixtures: %v", fx)
	}
	if v["specHash"] != b.SpecHash() || v["bundle"] != ref {
		t.Fatalf("specHash/bundle: %v %v", v["specHash"], v["bundle"])
	}
	perm := v["permissions"].(map[string]any)
	if perm["base"] != "none" || perm["widens"] != true {
		t.Fatalf("a new agent widens from none: %v", perm)
	}
	if p.Source.Path != "agents/echo-agent" || !strings.HasPrefix(p.Builder.Tool, "tapctl ") || p.Builder.Time != "2026-10-09T12:00:00Z" {
		t.Fatalf("source/builder: %+v %+v", p.Source, p.Builder)
	}
}

func TestBuildRefusesFailingFixtures(t *testing.T) {
	b, err := spec.Load("../../agents/echo-agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range []runner.Report{{Passed: 2, Failed: 1}, {}} {
		if _, err := Build(Input{Bundle: b, BundleRef: ref, Report: rep}); err == nil {
			t.Fatalf("expected refusal for %+v", rep)
		}
	}
}
