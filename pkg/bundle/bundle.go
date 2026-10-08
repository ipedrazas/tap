// Package bundle turns an agent source directory into a reproducible OCI image
// that the cluster mounts as an image volume.
//
// containerd only mounts directory subPaths of image volumes, so the source
// layout is projected into one directory per container:
//
//	harness/  agent.yaml, <prompt>, skills/<declared>
//	runner/   agent.yaml, tools/, mcp/, node_modules/, vendor/
//
// tests/ and anything else not listed stay in the repo.
package bundle

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/ipedrazas/tap/pkg/spec"
)

const (
	AnnotationSpecHash = "tap.hiddenfield.dev/spec-hash"
	AnnotationAgent    = "tap.hiddenfield.dev/agent"
)

// runnerDirs are copied whole into runner/ when present.
var runnerDirs = []string{"tools", "mcp", "node_modules", "vendor"}

// epoch is the mtime of every entry, so the digest depends on content only.
var epoch = time.Unix(0, 0).UTC()

type entry struct {
	dst string // path inside the image
	src string // file on disk; empty for directories
}

// Files lists the image contents (destination -> source), sorted.
func Files(b *spec.Bundle) ([]entry, error) {
	a := b.Agent
	var out []entry
	add := func(dst, src string) { out = append(out, entry{dst: dst, src: src}) }

	add("harness/agent.yaml", "agent.yaml")
	add("runner/agent.yaml", "agent.yaml")
	add(path.Join("harness", a.Prompt), a.Prompt)
	for _, s := range a.Skills {
		if err := walk(b.Dir, s, "harness", add); err != nil {
			return nil, err
		}
	}
	for _, d := range runnerDirs {
		if _, err := os.Stat(filepath.Join(b.Dir, d)); os.IsNotExist(err) {
			continue
		}
		if err := walk(b.Dir, d, "runner", add); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dst < out[j].dst })
	return out, nil
}

func walk(root, rel, prefix string, add func(dst, src string)) error {
	return filepath.WalkDir(filepath.Join(root, rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(root, p)
		r = filepath.ToSlash(r)
		base := d.Name()
		if base == ".DS_Store" || strings.HasPrefix(base, "._") {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlinks are not allowed in bundles", r)
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed in bundles", r)
		}
		if !d.IsDir() {
			add(path.Join(prefix, r), r)
		}
		return nil
	})
}

// Tar writes the projected bundle as an uncompressed, reproducible tar:
// sorted entries, uid/gid 0, fixed mtimes, no xattrs, files 0644 and dirs 0755.
func Tar(b *spec.Bundle) ([]byte, error) {
	files, err := Files(b)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	writeDir := func(d string) error {
		hdr := &tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: epoch, Format: tar.FormatPAX}
		return tw.WriteHeader(hdr)
	}
	for _, f := range files {
		// Parent directories first, each once.
		var parents []string
		for d := path.Dir(f.dst); d != "." && !dirs[d]; d = path.Dir(d) {
			parents = append(parents, d)
		}
		for i := len(parents) - 1; i >= 0; i-- {
			dirs[parents[i]] = true
			if err := writeDir(parents[i]); err != nil {
				return nil, err
			}
		}
		data, err := os.ReadFile(filepath.Join(b.Dir, f.src))
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.dst, Mode: 0o644, Size: int64(len(data)), ModTime: epoch, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Image builds the OCI image for b. Identical sources give identical digests.
func Image(b *spec.Bundle) (v1.Image, error) {
	data, err := Tar(b)
	if err != nil {
		return nil, err
	}
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img, err = mutate.Append(img, mutate.Addendum{
		Layer:     static.NewLayer(data, types.OCIUncompressedLayer),
		MediaType: types.OCIUncompressedLayer,
		History:   v1.History{CreatedBy: "tapctl bundle build", Created: v1.Time{Time: epoch}},
	})
	if err != nil {
		return nil, err
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg = cfg.DeepCopy()
	cfg.OS, cfg.Architecture = "linux", "amd64"
	cfg.Created = v1.Time{Time: epoch}
	cfg.Config.Labels = map[string]string{
		"org.opencontainers.image.title":   b.Agent.Metadata.Name,
		"org.opencontainers.image.version": b.Agent.Metadata.Version,
	}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return nil, err
	}
	return mutate.Annotations(img, map[string]string{
		"org.opencontainers.image.title":   b.Agent.Metadata.Name,
		"org.opencontainers.image.version": b.Agent.Metadata.Version,
		"org.opencontainers.image.created": epoch.Format(time.RFC3339),
		AnnotationAgent:                    b.Agent.Metadata.Name,
		AnnotationSpecHash:                 b.SpecHash(),
	}).(v1.Image), nil
}
