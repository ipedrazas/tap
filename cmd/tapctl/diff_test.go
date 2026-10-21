package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// diffRepo commits testdata's echo-agent into a fresh git repository and
// returns the agent's directory there.
func diffRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	repo := t.TempDir()
	dir := filepath.Join(repo, "agents", "echo-agent")
	if err := os.CopyFS(dir, os.DirFS("../../testdata/agents/echo-agent")); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	return dir
}

// edit rewrites a file in the agent directory.
func edit(t *testing.T, dir, file, old, new string) {
	t.Helper()
	p := filepath.Join(dir, file)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s has no %q", file, old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func diffCode(t *testing.T, base, dir string) int {
	t.Helper()
	err := cmdDiff([]string{"--platform", "../../platform.yaml", "--base", base, dir})
	var ee *exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	}
	t.Logf("diff: %v", err)
	return -1
}

func TestDiffExitCodes(t *testing.T) {
	bump := func(t *testing.T, dir string) { edit(t, dir, "agent.yaml", "version: 0.1.2", "version: 0.1.3") }
	for _, c := range []struct {
		name string
		edit func(t *testing.T, dir string)
		want int
	}{
		{"unchanged", func(*testing.T, string) {}, 0},
		{"code changed", func(t *testing.T, dir string) { bump(t, dir); edit(t, dir, "system.md", "", "Be brief.\n") }, 4},
		{"code changed, version not bumped", func(t *testing.T, dir string) { edit(t, dir, "system.md", "", "Be brief.\n") }, 1},
		{"owner changed", func(t *testing.T, dir string) {
			bump(t, dir)
			edit(t, dir, "agent.yaml", "owner: platform", "owner: other")
		}, 3},
		// The widening isn't masked by the missing bump.
		{"owner changed, version not bumped", func(t *testing.T, dir string) { edit(t, dir, "agent.yaml", "owner: platform", "owner: other") }, 3},
		{"runner changed", func(t *testing.T, dir string) { bump(t, dir); edit(t, dir, "agent.yaml", "sha256:43a4", "sha256:43a5") }, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := diffRepo(t)
			c.edit(t, dir)
			if got := diffCode(t, "git:main", dir); got != c.want {
				t.Fatalf("exit %d, want %d", got, c.want)
			}
		})
	}
	t.Run("base that doesn't resolve", func(t *testing.T) {
		if got := diffCode(t, "git:mian", diffRepo(t)); got != -1 {
			t.Fatalf("exit %d, want an error", got)
		}
	})
}
