package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ipedrazas/tap/pkg/attest"
	"github.com/ipedrazas/tap/pkg/bundle"
	"github.com/ipedrazas/tap/pkg/render"
	"github.com/ipedrazas/tap/pkg/runner"
	"github.com/ipedrazas/tap/pkg/spec"
)

func cmdAttest(args []string) error {
	fs, platform := newFlags("attest")
	ref := fs.String("bundle", "", "pushed bundle reference, by digest")
	refFile := fs.String("bundle-file", "", "read the bundle reference from this file")
	testLog := fs.String("test-log", "", "output of `tap-runner test` for this bundle (the fixture Job's log)")
	env := fs.String("environment", "", "where the fixtures ran, e.g. cluster:tap-ci/<job>")
	base := fs.String("base", "", "base version for the permission diff (as for tapctl diff); empty means a new agent")
	builder := fs.String("builder", "local", "identity of the pipeline producing the attestation")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *refFile != "" {
		data, err := os.ReadFile(*refFile)
		if err != nil {
			return err
		}
		*ref = strings.TrimSpace(string(data))
	}
	if *ref == "" || *testLog == "" {
		return fmt.Errorf("attest: --bundle (or --bundle-file) and --test-log are required")
	}
	b, _, err := validated(dir, *platform)
	if err != nil {
		return err
	}
	// The predicate describes the sources in dir, so they must be exactly
	// what was pushed: the build is reproducible, so compare digests.
	img, err := bundle.Image(b)
	if err != nil {
		return err
	}
	digest, err := img.Digest()
	if err != nil {
		return err
	}
	if !strings.HasSuffix(*ref, "@"+digest.String()) {
		return fmt.Errorf("attest: %s builds to %s, not %s; rebuild and retest", dir, digest, *ref)
	}
	out, err := os.ReadFile(*testLog)
	if err != nil {
		return err
	}
	rep, err := runner.ParseReport(out)
	if err != nil {
		return fmt.Errorf("attest: %s: %w", *testLog, err)
	}
	var baseBundle *spec.Bundle
	if *base != "" {
		raw, err := readBase(*base, dir)
		if err != nil {
			return err
		}
		if raw != nil {
			a, j, err := spec.ParseAgent(raw)
			if err != nil {
				return fmt.Errorf("base: %w", err)
			}
			baseBundle = &spec.Bundle{Agent: a, Raw: j}
		}
	}
	baseName := *base
	if baseBundle == nil {
		baseName = ""
	}
	p, err := attest.Build(attest.Input{
		Bundle: b, Dir: dir, BundleRef: *ref, Report: rep, Environment: *env,
		Base: baseName, Diff: spec.Diff(baseBundle, b), BuilderID: *builder, Now: time.Now(),
	})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

func cmdAdmissionPolicy(args []string) error {
	fs, platform := newFlags("admission policy")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := spec.LoadPlatform(*platform)
	if err != nil {
		return err
	}
	out, err := render.Admission(p)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}
