package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ipedrazas/tap/pkg/egress"
	"github.com/ipedrazas/tap/pkg/spec"
	"github.com/ipedrazas/tap/pkg/version"
)

// command is one tapctl subcommand. Its help (and docs/cli.md) is built from
// these fields plus the flags the command registers.
type command struct {
	name    string // one or two words, e.g. "bundle build"
	args    string // positional arguments, shown after the flags
	group   string
	summary string // one line
	long    string // paragraphs; may include examples
	run     func(args []string) error
}

var groups = []string{"Authoring", "Building and deploying", "MCP", "Cluster", "Platform", "Other"}

var commands []command

func init() {
	commands = []command{
		{name: "new", args: "<name>", group: "Authoring", run: cmdNew,
			summary: "Scaffold a new agent bundle that validates as-is",
			long: `Writes agents/<name>/ from the runner-node or runner-python scaffold, pinned to
the current curated runner digest from platform.yaml. Replace the example tool.

  tapctl new exchange-agent --runner runner-node --description "Converts currencies"`},
		{name: "validate", args: "<agent-dir>", group: "Authoring", run: cmdValidate,
			summary: "Check agent.yaml against the schema and rules 0-6, 8-12",
			long: `Loads agent.yaml, fixtures and MCP snapshots and applies every validation rule
(see docs/implementation-plan.md and .claude/skills/new-agent/reference/spec.md).
Prints one line per finding and exits 1 if there are any.`},
		{name: "diff", args: "<agent-dir>", group: "Authoring", run: cmdDiff,
			summary: "Permission diff against a base version (rules 7, 11)",
			long: `Compares tools, effects, secrets, egress hosts, MCP servers and effectsPolicy
with a base version. "+" lines widen permissions, "-" lines narrow them.

Exit codes: 0 no widening, 1 agent.yaml changed without a version bump,
3 permissions widened (human review required; expected for a new agent).

  tapctl diff --base git:origin/main agents/weather-agent
  tapctl diff --base oci:registry.hiddenfield.dev/agents/weather-agent:0.1.0 agents/weather-agent`},
		{name: "secrets", args: "<agent-dir>", group: "Authoring", run: cmdSecrets,
			summary: "List the secret names the agent declares, one per line"},
		{name: "runner bump", args: "<agent-dir>", group: "Authoring", run: cmdRunnerBump,
			summary: "Point the agent at the current curated runner digest",
			long:    `Rewrites runner.image in agent.yaml to the platform.yaml digest for the same runner. Bump metadata.version afterwards.`},
		{name: "egress policy", args: "<agent-dir>", group: "Authoring", run: cmdEgressPolicy,
			summary: "Print the agent's egress allowlist as the proxy reads it"},
		{name: "bundle build", args: "<agent-dir>", group: "Building and deploying", run: cmdBuild,
			summary: "Build the bundle image (reproducible); --push to upload it",
			long: `Validates, then builds the OCI image the cluster mounts as an image volume.
Identical sources give identical digests. With --push the image goes to
<registry>/agents/<name>:<version>; version tags are immutable. --dev pushes to
the moving :dev tag instead.`},
		{name: "render", args: "<agent-dir>", group: "Building and deploying", run: cmdRender,
			summary: "Print the Kubernetes manifests for a pushed bundle",
			long:    `Output contains no secret values and can be committed to a GitOps repo. --test renders the fixture Job instead.`},
		{name: "mcp list", args: "<agent-dir>", group: "MCP", run: cmdMCPList,
			summary: "List every tool each MCP server offers (* = allowlisted)"},
		{name: "mcp snapshot", args: "<agent-dir>", group: "MCP", run: cmdMCPSnapshot,
			summary: "Pin allowlisted MCP tool schemas into mcp/<server>.tools.json",
			long:    `Connects to each declared server and pins the allowlisted tools' descriptions and input schemas. With --check, reports drift instead of writing (exit 1 on drift).`},
		{name: "mcp call", args: "<agent-dir> <server>__<tool> '<json args>'", group: "MCP", run: cmdMCPCall,
			summary: "Make one live MCP call and print the raw result (for fixtures)"},
		{name: "agent ls", group: "Cluster", run: cmdAgentLs,
			summary: "List the agents running in the cluster",
			long: `Reads agent namespaces through your kubeconfig and prints one line per agent:
readiness, version, model, tool counts, restarts, age and URL.

  tapctl agent ls
  tapctl agent ls -o json`},
		{name: "agent get", args: "<name>", group: "Cluster", run: cmdAgentGet,
			summary: "Show one running agent in detail (tools, egress, bundle, pods)"},
		{name: "platform pin", args: "<harness|egress-proxy|runner-name> <image@sha256:...>", group: "Platform", run: cmdPlatformPin,
			summary: "Record an image digest in platform.yaml"},
		{name: "version", group: "Other", run: cmdVersion,
			summary: "Print version, git commit and build date"},
		{name: "help", args: "[command]", group: "Other", run: cmdHelp,
			summary: "Show help for tapctl or one command"},
	}
}

// helpOut receives flag help; docs generation captures it.
var helpOut io.Writer = os.Stderr

// lookup finds the command named by the first one or two args.
func lookup(args []string) (*command, []string) {
	for _, n := range []int{2, 1} {
		if len(args) < n {
			continue
		}
		name := strings.Join(args[:n], " ")
		for i := range commands {
			if commands[i].name == name {
				return &commands[i], args[n:]
			}
		}
	}
	return nil, args
}

func (c *command) usageLine() string {
	s := "tapctl " + c.name + " [flags]"
	if c.args != "" {
		s += " " + c.args
	}
	return s
}

// printHelp writes a command's full help, flags included.
func (c *command) printHelp(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "%s\n\nUsage:\n  %s\n", c.summary, c.usageLine())
	if c.long != "" {
		fmt.Fprintf(w, "\n%s\n", c.long)
	}
	if fs != nil {
		var flags bytes.Buffer
		fs.SetOutput(&flags)
		fs.PrintDefaults()
		if flags.Len() > 0 {
			fmt.Fprintf(w, "\nFlags:\n%s", flags.String())
		}
	}
}

func overview(w io.Writer) {
	fmt.Fprintf(w, "tapctl builds, checks and inspects tap agents.\n\nUsage:\n  tapctl <command> [flags] [args]\n")
	width := 0
	for _, c := range commands {
		width = max(width, len(c.name))
	}
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s:\n", g)
		for _, c := range commands {
			if c.group == g {
				fmt.Fprintf(w, "  %-*s  %s\n", width, c.name, c.summary)
			}
		}
	}
	fmt.Fprintf(w, "\nRun \"tapctl help <command>\" or \"tapctl <command> -h\" for flags and examples.\nGlobal: --version prints the build. Reference: docs/cli.md\n")
}

// newFlags returns a FlagSet whose -h prints the command's full help.
func newFlags(name string) (*flag.FlagSet, *string) {
	fs := newFlagsPlain(name)
	platform := fs.String("platform", "platform.yaml", "platform config")
	return fs, platform
}

func newFlagsPlain(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(helpOut)
	fs.Usage = func() {
		for i := range commands {
			if commands[i].name == name {
				commands[i].printHelp(helpOut, fs)
				return
			}
		}
	}
	return fs
}

func cmdHelp(args []string) error {
	if len(args) == 0 {
		overview(os.Stdout)
		return nil
	}
	c, _ := lookup(args)
	if c == nil {
		return fmt.Errorf("unknown command %q", strings.Join(args, " "))
	}
	// Commands print their own help (with flags) on -h.
	old := helpOut
	helpOut = os.Stdout
	defer func() { helpOut = old }()
	if err := c.run([]string{"-h"}); err != nil && !errors.Is(err, flag.ErrHelp) {
		c.printHelp(os.Stdout, nil)
	}
	return nil
}

func cmdVersion(args []string) error {
	fs := newFlagsPlain("version")
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(version.Get())
	}
	fmt.Println(version.Get())
	return nil
}

func cmdEgressPolicy(args []string) error {
	fs := newFlagsPlain("egress policy")
	dir, err := parse(fs, args)
	if err != nil {
		return err
	}
	b, err := spec.Load(dir)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(egress.PolicyFor(b.Agent))
}

func cmdPlatformPin(args []string) error {
	fs := newFlagsPlain("platform pin")
	file := fs.String("file", "platform.yaml", "platform config to edit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return &exitError{code: 1}
	}
	return spec.PinImage(*file, fs.Arg(0), fs.Arg(1))
}

// markdown renders docs/cli.md from the command table.
func markdown() string {
	var b strings.Builder
	b.WriteString("# tapctl reference\n\nGenerated by `task docs:cli` from `cmd/tapctl/commands.go`; do not edit by hand.\n\n")
	b.WriteString("Every command prints this help with `tapctl help <command>` or `tapctl <command> -h`. `tapctl --version` (or `tapctl version`) prints the version, git commit and build date.\n\n")
	b.WriteString("Exit codes: `0` ok, `1` error or failed check, `3` (`diff` only) permissions widened.\n")
	for _, g := range groups {
		fmt.Fprintf(&b, "\n## %s\n", g)
		for i := range commands {
			c := &commands[i]
			if c.group != g || c.name == "help" {
				continue
			}
			var help bytes.Buffer
			old := helpOut
			helpOut = &help
			if err := c.run([]string{"-h"}); err != nil && !errors.Is(err, flag.ErrHelp) || help.Len() == 0 {
				help.Reset()
				c.printHelp(&help, nil)
			}
			helpOut = old
			fmt.Fprintf(&b, "\n### `tapctl %s`\n\n```text\n%s```\n", c.name, help.String())
		}
	}
	return b.String()
}
