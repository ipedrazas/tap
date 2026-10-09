// Package attest builds the in-toto predicate signed onto every tested
// bundle: what the bundle is (spec hash, images), that its fixtures passed in
// the real runner image, how its permissions differ from a base version, and
// who built it. Admission requires a verified predicate with passing fixtures.
package attest

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/ipedrazas/tap/pkg/runner"
	"github.com/ipedrazas/tap/pkg/spec"
	"github.com/ipedrazas/tap/pkg/version"
)

// Predicate is the body of the in-toto statement (platform.yaml
// signing.predicateType). `task agent:verify` reads its fixtures fields.
type Predicate struct {
	Agent       Agent       `json:"agent"`
	Bundle      string      `json:"bundle"`
	SpecHash    string      `json:"specHash"`
	Images      Images      `json:"images"`
	Fixtures    Fixtures    `json:"fixtures"`
	Permissions Permissions `json:"permissions"`
	Builder     Builder     `json:"builder"`
	Source      Source      `json:"source"`
}

type Agent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Owner   string `json:"owner"`
}

type Images struct {
	Runner string `json:"runner"`
}

type Fixtures struct {
	// OK is true when at least one case passed and none failed.
	OK bool `json:"ok"`
	runner.Report
	// Environment says where they ran, e.g. "cluster:tap-ci/test-echo-agent-0123456789".
	Environment string `json:"environment"`
}

type Permissions struct {
	// Base is what the bundle was compared with (git:<ref>, oci:<ref>, a
	// file, or "none" for a new agent).
	Base          string             `json:"base"`
	Widens        bool               `json:"widens"`
	Changes       []string           `json:"changes"`
	EffectsPolicy spec.EffectsPolicy `json:"effectsPolicy"`
}

type Builder struct {
	// ID names the pipeline that signed; Tool is the tapctl build.
	ID   string `json:"id"`
	Tool string `json:"tool"`
	User string `json:"user"`
	Host string `json:"host"`
	Time string `json:"time"`
}

type Source struct {
	Repository string `json:"repository,omitempty"`
	Commit     string `json:"commit,omitempty"`
	// Dirty is set when the agent directory has uncommitted changes.
	Dirty bool   `json:"dirty"`
	Path  string `json:"path"`
}

type Input struct {
	Bundle      *spec.Bundle
	Dir         string // agent directory, for git provenance
	BundleRef   string // pushed bundle, by digest
	Report      runner.Report
	Environment string
	Base        string
	Diff        spec.DiffResult
	BuilderID   string
	Now         time.Time
}

func Build(in Input) (*Predicate, error) {
	if !in.Report.OK() {
		return nil, errors.New("fixtures did not pass; only passing runs are attested")
	}
	a := in.Bundle.Agent
	p := &Predicate{
		Agent:    Agent{Name: a.Metadata.Name, Version: a.Metadata.Version, Owner: a.Metadata.Owner},
		Bundle:   in.BundleRef,
		SpecHash: in.Bundle.SpecHash(),
		Images:   Images{Runner: a.Runner.Image},
		Fixtures: Fixtures{OK: true, Report: in.Report, Environment: in.Environment},
		Permissions: Permissions{
			Base:          or(in.Base, "none"),
			Widens:        in.Diff.Widens(),
			Changes:       []string{},
			EffectsPolicy: a.EffectsPolicy,
		},
		Builder: Builder{
			ID:   or(in.BuilderID, "local"),
			Tool: "tapctl " + version.Get().Short(),
			User: identity(),
			Host: hostname(),
			Time: in.Now.UTC().Format(time.RFC3339),
		},
		Source: source(in.Dir),
	}
	for _, c := range in.Diff.Changes {
		p.Permissions.Changes = append(p.Permissions.Changes, c.String())
	}
	return p, nil
}

// identity is the git author, the closest thing to a person we have locally.
func identity() string {
	if out, err := exec.Command("git", "config", "user.email").Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return strings.TrimSpace(string(out))
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func source(dir string) Source {
	s := Source{Path: dir}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	s.Repository = git("remote", "get-url", "origin")
	s.Commit = git("rev-parse", "HEAD")
	if top := git("rev-parse", "--show-toplevel"); top != "" {
		if rel := git("ls-files", "--full-name", "--", "agent.yaml"); rel != "" {
			s.Path = strings.TrimSuffix(rel, "/agent.yaml")
		}
		s.Dirty = git("status", "--porcelain", "--", ".") != ""
	}
	return s
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
