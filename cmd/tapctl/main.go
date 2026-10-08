// tapctl validates, diffs, builds and renders agent bundles.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ipedrazas/tap/pkg/bundle"
	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/render"
	"github.com/ipedrazas/tap/pkg/spec"
)

const usage = `tapctl <command> [flags] <agent-dir>

Commands:
  validate        schema + rules 0-6, 8-12
  diff            permission diff against a base version (rules 7, 11)
  bundle build    build the bundle image; --push to upload it
  render          print Kubernetes manifests for a pushed bundle
  secrets         list the secret names the agent declares, one per line
  runner bump     point the agent at the current curated runner digest
  egress policy   print the agent's egress allowlist as the proxy reads it
  platform pin    platform pin <harness|runner-name> <image@sha256:...>

Exit codes: 0 ok, 1 error or failed validation, 3 diff widens permissions (needs review).
`

// exitError carries a specific exit code up to main.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func main() {
	if err := run(os.Args[1:]); err != nil {
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
		fmt.Fprint(os.Stderr, usage)
		return &exitError{code: 1}
	}
	cmd, rest := args[0], args[1:]
	if cmd == "bundle" {
		if len(rest) == 0 || rest[0] != "build" {
			return fmt.Errorf("usage: tapctl bundle build [flags] <agent-dir>")
		}
		cmd, rest = "bundle build", rest[1:]
	}
	switch cmd {
	case "validate":
		return cmdValidate(rest)
	case "diff":
		return cmdDiff(rest)
	case "bundle build":
		return cmdBuild(rest)
	case "render":
		return cmdRender(rest)
	case "secrets":
		return cmdSecrets(rest)
	case "runner":
		if len(rest) == 0 || rest[0] != "bump" {
			return fmt.Errorf("usage: tapctl runner bump <agent-dir>")
		}
		return cmdRunnerBump(rest[1:])
	case "egress":
		if len(rest) != 2 || rest[0] != "policy" {
			return fmt.Errorf("usage: tapctl egress policy <agent-dir>")
		}
		b, err := spec.Load(rest[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(egress.PolicyFor(b.Agent))
	case "platform":
		if len(rest) != 3 || rest[0] != "pin" {
			return fmt.Errorf("usage: tapctl platform pin <harness|runner-name> <image@sha256:...>")
		}
		return spec.PinImage("platform.yaml", rest[1], rest[2])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	platform := fs.String("platform", "platform.yaml", "platform config")
	return fs, platform
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
	d := spec.Diff(baseBundle, head)
	if baseBundle == nil {
		fmt.Println("new agent; every permission is new:")
	}
	for _, c := range d.Changes {
		fmt.Println(c)
	}
	if len(d.Changes) == 0 {
		fmt.Println("no permission changes")
	}
	if d.VersionUnchanged {
		return &exitError{code: 1, msg: "agent.yaml changed but metadata.version did not; bump it"}
	}
	if d.Widens() {
		return &exitError{code: 3, msg: "permissions widened: human review required"}
	}
	return nil
}

// readBase returns nil, nil when the base does not exist (a new agent).
func readBase(base, dir string) ([]byte, error) {
	switch {
	case strings.HasPrefix(base, "git:"):
		ref := strings.TrimPrefix(base, "git:")
		top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return nil, fmt.Errorf("git: %w", err)
		}
		abs, err := filepath.Abs(filepath.Join(dir, "agent.yaml"))
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(strings.TrimSpace(string(top)), abs)
		if err != nil {
			return nil, err
		}
		out, err := exec.Command("git", "-C", dir, "show", ref+":"+filepath.ToSlash(rel)).Output()
		if err != nil {
			// Not present at ref: a new agent.
			return nil, nil
		}
		return out, nil
	case strings.HasPrefix(base, "oci:"):
		return bundle.PullAgentYAML(strings.TrimPrefix(base, "oci:"))
	default:
		return os.ReadFile(base)
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
	out, err := renderFn(render.Input{Bundle: b, Platform: p, BundleRef: *ref})
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

func cmdSecrets(args []string) error {
	fs, _ := newFlags("secrets")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	b, err := spec.Load(dir)
	if err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(b.Agent.Secrets))
	for _, n := range names {
		fmt.Println(n)
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
