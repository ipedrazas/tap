package bundle

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ipedrazas/tap/pkg/spec"
)

func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestReproducible(t *testing.T) {
	a := copyTree(t, "../../agents/echo-agent")
	b := copyTree(t, "../../agents/echo-agent")
	// Different mtimes, modes and macOS junk must not change the digest.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(b, "system.md"), later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "tools", "._echo.ts"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	da := digest(t, a)
	if db := digest(t, b); da != db {
		t.Fatalf("digests differ: %s vs %s", da, db)
	}
}

func digest(t *testing.T, dir string) string {
	t.Helper()
	bundle, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Image(bundle)
	if err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}

func TestProjection(t *testing.T) {
	bundle, err := spec.Load("../../agents/echo-agent")
	if err != nil {
		t.Fatal(err)
	}
	data, err := Tar(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 || !hdr.ModTime.Equal(epoch) {
			t.Errorf("%s: non-reproducible header %+v", hdr.Name, hdr)
		}
		names = append(names, hdr.Name)
	}
	want := []string{
		"harness/", "harness/agent.yaml", "harness/skills/", "harness/skills/echo/", "harness/skills/echo/SKILL.md", "harness/system.md",
		"runner/", "runner/agent.yaml", "runner/tools/", "runner/tools/echo.ts", "runner/tools/time.ts",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("got %v\nwant %v", names, want)
	}
}

func TestRejectsSymlink(t *testing.T) {
	dir := copyTree(t, "../../agents/echo-agent")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "tools", "leak.ts")); err != nil {
		t.Fatal(err)
	}
	bundle, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Tar(bundle); err == nil {
		t.Fatal("expected symlink error")
	}
}
