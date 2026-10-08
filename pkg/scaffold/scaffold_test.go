package scaffold

import (
	"path/filepath"
	"testing"

	"github.com/ipedrazas/tap/pkg/spec"
)

// A fresh scaffold must pass validation for every supported runner.
func TestScaffoldValidates(t *testing.T) {
	p, err := spec.LoadPlatform("../../platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, runner := range []string{"runner-node", "runner-python"} {
		t.Run(runner, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "demo-agent")
			if err := New(Options{Dir: dir, Name: "demo-agent", Owner: "me", Description: "Demo agent", Runner: runner}, p); err != nil {
				t.Fatal(err)
			}
			b, err := spec.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if f := spec.Validate(b, p); len(f) != 0 {
				t.Fatalf("scaffold does not validate: %v", f)
			}
		})
	}
}
