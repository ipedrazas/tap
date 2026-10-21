// tapctl validates, diffs, builds and renders agent bundles.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ipedrazas/tap/pkg/bundle"
	"github.com/ipedrazas/tap/pkg/render"
	"github.com/ipedrazas/tap/pkg/scaffold"
	"github.com/ipedrazas/tap/pkg/spec"
)

// exitError carries a specific exit code up to main.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, ee.msg)
			}
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "tapctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		overview(os.Stderr)
		return &exitError{code: 1}
	}
	switch args[0] {
	case "-h", "--help":
		overview(os.Stdout)
		return nil
	case "--version", "-v":
		return cmdVersion(nil)
	case "__docs":
		fmt.Print(markdown())
		return nil
	}
	c, rest := lookup(args)
	if c == nil {
		return fmt.Errorf("unknown command %q; run \"tapctl help\"", strings.Join(args[:min(2, len(args))], " "))
	}
	return c.run(rest)
}

// parse handles flags before or after the agent directory.
func parse(fs *flag.FlagSet, args []string) (string, error) {
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	if len(positional) != 1 {
		return "", fmt.Errorf("%s: expected one agent directory", fs.Name())
	}
	return positional[0], nil
}

func loadAll(dir, platformPath string) (*spec.Bundle, *spec.Platform, error) {
	p, err := spec.LoadPlatform(platformPath)
	if err != nil {
		return nil, nil, err
	}
	b, err := spec.Load(dir)
	if err != nil {
		return nil, nil, err
	}
	return b, p, nil
}

func validated(dir, platformPath string) (*spec.Bundle, *spec.Platform, error) {
	b, p, err := loadAll(dir, platformPath)
	if err != nil {
		return nil, nil, err
	}
	if findings := spec.Validate(b, p); len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f)
		}
		return nil, nil, &exitError{code: 1, msg: fmt.Sprintf("%s: %d finding(s)", dir, len(findings))}
	}
	return b, p, nil
}

func cmdValidate(args []string) error {
	fs, platform := newFlags("validate")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	b, _, err := validated(dir, *platform)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s: ok (%d tools, %d mcp servers)\n", b.Agent.Metadata.Name, b.Agent.Metadata.Version, len(b.Agent.Tools), len(b.Agent.MCP))
	return nil
}

func cmdDiff(args []string) error {
	fs, platform := newFlags("diff")
	base := fs.String("base", "", "base version: git:<ref>, oci:<image-ref>, or a path to agent.yaml; empty means a new agent")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	head, _, err := loadAll(dir, *platform)
	if err != nil {
		return err
	}
	var baseBundle *spec.Bundle
	var baseFiles map[string]string
	if *base != "" {
		raw, files, err := readBase(*base, dir)
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
		baseFiles = files
	}
	d := spec.Diff(baseBundle, head)
	if baseBundle != nil && baseFiles != nil {
		headFiles, err := agentFiles(dir)
		if err != nil {
			return err
		}
		d.DiffFiles(baseBundle, head, baseFiles, headFiles)
	}
	ep := head.Agent.EffectsPolicy
	fmt.Printf("%s %s; effectsPolicy: write=%s irreversible=%s\n", head.Agent.Metadata.Name, head.Agent.Metadata.Version, ep.Write, ep.Irreversible)
	if baseBundle == nil {
		fmt.Println("new agent; every permission is new:")
	}
	for _, c := range d.Changes {
		fmt.Println(c)
	}
	if len(d.Changes) == 0 {
		fmt.Println("no permission changes")
	}
	if baseBundle != nil && baseFiles == nil {
		fmt.Println("~ files other than agent.yaml not compared (only a git: base has them)")
	}
	// Widening outranks the version check, so the review it needs isn't
	// masked; both are reported.
	const bump = "the bundle changed but metadata.version did not; bump it"
	if d.Widens() {
		msg := "permissions widened: human review required"
		if d.VersionUnchanged {
			msg += "; also, " + bump
		}
		return &exitError{code: 3, msg: msg}
	}
	if d.VersionUnchanged {
		return &exitError{code: 1, msg: bump}
	}
	if d.CodeChanged {
		return &exitError{code: 4, msg: "files outside agent.yaml changed: review the code"}
	}
	return nil
}

// agentFiles hashes every file in an agent directory but agent.yaml, as git
// blob ids, so they compare with a git: base.
func agentFiles(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() || name == ".DS_Store" || strings.HasPrefix(name, "._") {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "agent.yaml" {
			return nil
		}
		if !d.Type().IsRegular() {
			out[rel] = "not a regular file"
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha1.New()
		fmt.Fprintf(h, "blob %d\x00", len(data))
		h.Write(data)
		out[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out, err
}

// readBase returns the base agent.yaml, or nil when the agent doesn't exist
// at the base (a new agent). For a git: base it also returns the agent
// directory's other files there, as agentFiles does; nil for other bases.
// A git ref that doesn't resolve (a typo, a shallow clone) is an error, not a
// new agent.
func readBase(base, dir string) ([]byte, map[string]string, error) {
	switch {
	case strings.HasPrefix(base, "git:"):
		ref := strings.TrimPrefix(base, "git:")
		if err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run(); err != nil {
			return nil, nil, fmt.Errorf("base %s: no such commit in this clone (fetch it, or check the name)", base)
		}
		top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return nil, nil, fmt.Errorf("git: %w", err)
		}
		// git prints the top level with symlinks resolved (macOS's /var).
		abs, err := filepath.EvalSymlinks(dir)
		if err == nil {
			abs, err = filepath.Abs(abs)
		}
		if err != nil {
			return nil, nil, err
		}
		rel, err := filepath.Rel(strings.TrimSpace(string(top)), abs)
		if err != nil {
			return nil, nil, err
		}
		rel = filepath.ToSlash(rel)
		out, err := exec.Command("git", "-C", dir, "show", ref+":"+rel+"/agent.yaml").Output()
		if err != nil {
			// Not present at ref: a new agent.
			return nil, nil, nil
		}
		tree, err := exec.Command("git", "-C", strings.TrimSpace(string(top)), "ls-tree", "-r", "-z", "--full-name", ref, "--", rel+"/").Output()
		if err != nil {
			return nil, nil, fmt.Errorf("git ls-tree: %w", err)
		}
		files := map[string]string{}
		for _, line := range strings.Split(string(tree), "\x00") {
			meta, path, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			f := strings.TrimPrefix(path, rel+"/")
			if f == "agent.yaml" {
				continue
			}
			fields := strings.Fields(meta) // mode type object
			if len(fields) != 3 || fields[1] != "blob" || fields[0] == "120000" {
				files[f] = "not a regular file"
				continue
			}
			files[f] = fields[2]
		}
		return out, files, nil
	case strings.HasPrefix(base, "oci:"):
		raw, err := bundle.PullAgentYAML(strings.TrimPrefix(base, "oci:"))
		return raw, nil, err
	default:
		raw, err := os.ReadFile(base)
		return raw, nil, err
	}
}

func cmdBuild(args []string) error {
	fs, platform := newFlags("bundle build")
	push := fs.Bool("push", false, "push to <registry>/agents/<name>:<version>")
	dev := fs.Bool("dev", false, "with --push, use the moving tag :dev instead of the immutable version tag")
	digestFile := fs.String("digest-file", "", "write the pushed digest reference to this file")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	b, p, err := validated(dir, *platform)
	if err != nil {
		return err
	}
	img, err := bundle.Image(b)
	if err != nil {
		return err
	}
	digest, err := img.Digest()
	if err != nil {
		return err
	}
	repo := bundle.Repository(p, b.Agent.Metadata.Name)
	if !*push {
		fmt.Printf("%s@%s\n", repo, digest)
		return nil
	}
	ref, err := bundle.Push(img, repo, b.Agent.Metadata.Version, *dev)
	if err != nil {
		return err
	}
	if *digestFile != "" {
		if err := os.MkdirAll(filepath.Dir(*digestFile), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*digestFile, []byte(ref.String()+"\n"), 0o644); err != nil {
			return err
		}
	}
	fmt.Println(ref.String())
	return nil
}

func cmdRender(args []string) error {
	fs, platform := newFlags("render")
	ref := fs.String("bundle", "", "pushed bundle reference, by digest")
	refFile := fs.String("bundle-file", "", "read the bundle reference from this file")
	test := fs.Bool("test", false, "render the fixture-test Job instead of the runtime manifests")
	preview := fs.Bool("preview", false, "render the agent's preview: <name>-dev in its own namespace, accepted by admission when signed as built")
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
	b, p, err := validated(dir, *platform)
	if err != nil {
		return err
	}
	renderFn := render.Render
	if *test {
		renderFn = render.RenderTest
	}
	out, err := renderFn(render.Input{Bundle: b, Platform: p, BundleRef: *ref, Preview: *preview})
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

func cmdSecrets(args []string) error {
	fs, platform := newFlags("secrets")
	paths := fs.Bool("paths", false, "print each name with its OpenBao path (<kvMount> <path>), as the seeding tasks use it")
	preview := fs.Bool("preview", false, "with --paths, the preview's paths (<name>-dev)")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	b, err := spec.Load(dir)
	if err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(b.Agent.Secrets))
	if !*paths {
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	}
	p, err := spec.LoadPlatform(*platform)
	if err != nil {
		return err
	}
	for _, n := range names {
		path, err := p.SecretPathFor(b.Agent, n, *preview)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s %s\n", n, p.Secrets.KVMount, path)
	}
	return nil
}

func cmdRunnerBump(args []string) error {
	fs, platform := newFlags("runner bump")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	p, err := spec.LoadPlatform(*platform)
	if err != nil {
		return err
	}
	from, to, err := spec.BumpRunner(filepath.Join(dir, "agent.yaml"), p)
	if err != nil {
		return err
	}
	if from == to {
		fmt.Println("runner image already current")
		return nil
	}
	fmt.Printf("runner.image %s\n          -> %s\n", from, to)
	return nil
}

func cmdNew(args []string) error {
	fs, platform := newFlags("new")
	runner := fs.String("runner", "runner-node", "runner-node or runner-python")
	owner := fs.String("owner", "platform", "owning team")
	desc := fs.String("description", "Describe what this agent does in one sentence", "one-sentence description (max 200 chars)")
	agentsDir := fs.String("agents-dir", "agents", "where agent bundles live")
	name, err := parse(fs, args)
	if err != nil {
		return err
	}
	p, err := spec.LoadPlatform(*platform)
	if err != nil {
		return err
	}
	dir := filepath.Join(*agentsDir, name)
	if err := scaffold.New(scaffold.Options{Dir: dir, Name: name, Owner: *owner, Description: *desc, Runner: *runner}, p); err != nil {
		return err
	}
	fmt.Printf("created %s from the %s scaffold; it validates as-is. Replace the example tool.\n", dir, *runner)
	return nil
}
