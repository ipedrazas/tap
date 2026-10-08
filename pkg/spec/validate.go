package spec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"
)

// ReservedToolNames are harness built-ins.
var ReservedToolNames = []string{"read_skill_file"}

// Finding is one rule violation. Rule 0 covers structural checks that the
// design doc's numbered rules take for granted (files exist, paths are safe).
type Finding struct {
	Rule int
	Path string
	Msg  string
}

func (f Finding) String() string { return fmt.Sprintf("rule %d: %s: %s", f.Rule, f.Path, f.Msg) }

const maxTimeout = 5 * time.Minute

var (
	templateRe = regexp.MustCompile(`^\{\{([a-z_][a-z0-9_]*)\}\}$`)
	// Interpreters that would let a tool run arbitrary inline code.
	shells = []string{"sh", "bash", "zsh", "dash", "ash", "ksh", "env", "busybox", "xargs"}
	// Flags that evaluate inline code or load arbitrary modules.
	inlineCodeFlags = []string{"-c", "-e", "--eval", "-p", "--print", "-m", "-r", "--require", "--import", "--loader", "-i"}
	// Interpreters whose exec[1] must be a script inside tools/.
	scriptInterpreters = []string{"node", "python3", "python"}
)

// Validate applies validation rules 0–6 and 8–12. Rule 7 (permission widening)
// needs a base version and lives in Diff.
func Validate(b *Bundle, p *Platform) []Finding {
	v := &validator{b: b, a: b.Agent, p: p}
	v.structure()
	runner := v.runnerImage()
	for i, t := range v.a.Tools {
		v.tool(fmt.Sprintf("tools[%d](%s)", i, t.Name), t, runner)
	}
	v.mcp()
	v.secrets()
	v.fixtures()
	return v.out
}

type validator struct {
	b   *Bundle
	a   *Agent
	p   *Platform
	out []Finding
}

func (v *validator) add(rule int, path, format string, args ...any) {
	v.out = append(v.out, Finding{Rule: rule, Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(v.b.Dir, rel))
	return err == nil
}

func (v *validator) structure() {
	a := v.a
	if !safeRel(a.Prompt) {
		v.add(0, "prompt", "path %q must be relative and inside the bundle", a.Prompt)
	} else if !v.exists(a.Prompt) {
		v.add(0, "prompt", "%s does not exist", a.Prompt)
	}
	if v.p != nil && !slices.Contains(v.p.Model.Routes, a.Harness.Model.Name) {
		v.add(0, "harness.model.name", "%q is not a model gateway route (have %v)", a.Harness.Model.Name, v.p.Model.Routes)
	}
	for i, s := range a.Skills {
		path := fmt.Sprintf("skills[%d]", i)
		md := filepath.Join(s, "SKILL.md")
		if !v.exists(md) {
			v.add(0, path, "%s does not exist", md)
			continue
		}
		fm, err := skillFrontmatter(filepath.Join(v.b.Dir, md))
		if err != nil {
			v.add(0, path, "%s: %v", md, err)
			continue
		}
		if fm.Name == "" || fm.Description == "" {
			v.add(0, path, "%s frontmatter needs name and description", md)
		}
	}
	v.duration("runner.timeout", a.Runner.Timeout)
}

func (v *validator) duration(path, s string) {
	if s == "" {
		return
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		v.add(0, path, "invalid duration %q", s)
	} else if d <= 0 || d > maxTimeout {
		v.add(0, path, "%s must be within (0, %s]", s, maxTimeout)
	}
}

// Rule 6: runner image pinned by digest and on the curated list.
func (v *validator) runnerImage() *RunnerImage {
	if v.p == nil {
		return nil
	}
	r, ok := v.p.Runner(v.a.Runner.Image)
	if !ok {
		var names []string
		for _, c := range v.p.Runners {
			if c.Image != "" {
				names = append(names, c.Image)
			}
		}
		v.add(6, "runner.image", "%q is not a curated runner image (platform.yaml has %v)", v.a.Runner.Image, names)
		return nil
	}
	return &r
}

func (v *validator) tool(path string, t Tool, runner *RunnerImage) {
	v.duration(path+".timeout", t.Timeout)
	if strings.Contains(t.Name, "__") {
		v.add(8, path+".name", "%q must not contain \"__\" (reserved for MCP tools)", t.Name)
	}
	if slices.Contains(ReservedToolNames, t.Name) {
		v.add(8, path+".name", "%q is a harness built-in", t.Name)
	}

	// Rule 1: exec[0] is a curated interpreter, never a shell, no inline code.
	interp := t.Exec[0]
	switch {
	case slices.Contains(shells, filepath.Base(interp)):
		v.add(1, path+".exec[0]", "%q is a shell; tools must run an interpreter on a script", interp)
	case strings.Contains(interp, "/"):
		v.add(1, path+".exec[0]", "%q must be a bare interpreter name resolved in the runner image", interp)
	case runner != nil && !slices.Contains(runner.Interpreters, interp):
		v.add(1, path+".exec[0]", "%q is not available in %s (has %v)", interp, runner.Name, runner.Interpreters)
	}
	for i, arg := range t.Exec[1:] {
		if slices.Contains(inlineCodeFlags, arg) {
			v.add(1, fmt.Sprintf("%s.exec[%d]", path, i+1), "flag %q evaluates inline code or loads arbitrary modules", arg)
		}
	}
	if slices.Contains(scriptInterpreters, interp) {
		script := t.Exec[1]
		switch {
		case !safeRel(script) || !strings.HasPrefix(script, "tools/"):
			v.add(1, path+".exec[1]", "%q must be a script under tools/", script)
		case !v.exists(script):
			v.add(1, path+".exec[1]", "%s does not exist", script)
		}
	}

	// Rule 2: every {{arg}} fills a whole argv slot and maps to a scalar,
	// constrained, always-present property.
	schemaPath := path + ".input_schema"
	if _, err := CompileSchema(t.Name+".input.json", t.InputSchema); err != nil {
		v.add(2, schemaPath, "invalid JSON Schema: %v", err)
		return
	}
	var is struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(t.InputSchema, &is); err != nil {
		v.add(2, schemaPath, "%v", err)
		return
	}
	used := map[string]bool{}
	for i, arg := range t.Exec {
		argPath := fmt.Sprintf("%s.exec[%d]", path, i)
		m := templateRe.FindStringSubmatch(arg)
		if m == nil {
			if strings.Contains(arg, "{{") || strings.Contains(arg, "}}") {
				v.add(2, argPath, "%q: a template must be the whole argv element, e.g. \"{{name}}\"", arg)
			}
			continue
		}
		if i == 0 {
			v.add(2, argPath, "exec[0] cannot be templated")
			continue
		}
		name := m[1]
		used[name] = true
		prop, ok := is.Properties[name]
		if !ok {
			v.add(2, argPath, "{{%s}} is not a property of input_schema", name)
			continue
		}
		typ, _ := prop["type"].(string)
		if !slices.Contains([]string{"string", "integer", "number", "boolean"}, typ) {
			v.add(2, argPath, "{{%s}} has type %q; only string, integer, number and boolean fit in one argv slot", name, typ)
		}
		if typ == "string" && prop["pattern"] == nil && prop["enum"] == nil && prop["const"] == nil && prop["format"] == nil {
			v.add(2, argPath, "{{%s}} is a string without pattern, enum, const or format; constrain it so a value cannot become a flag", name)
		}
		if !slices.Contains(is.Required, name) && prop["default"] == nil {
			v.add(2, argPath, "{{%s}} must be required or have a default, or its argv slot would be empty", name)
		}
	}
	for name := range is.Properties {
		if !used[name] {
			v.add(2, schemaPath+".properties."+name, "property is never substituted into exec; the tool could not receive it")
		}
	}

	// Rule 4: no wildcard or IP egress.
	for i, e := range t.Egress {
		host, port, err := net.SplitHostPort(e)
		ePath := fmt.Sprintf("%s.egress[%d]", path, i)
		switch {
		case err != nil:
			v.add(4, ePath, "%q must be host:port", e)
		case strings.Contains(host, "*"):
			v.add(4, ePath, "wildcard egress %q needs an explicit exception (not supported in v1)", e)
		case net.ParseIP(host) != nil:
			v.add(4, ePath, "%q: egress must name a host, not an IP", e)
		case port == "0":
			v.add(4, ePath, "%q: invalid port", e)
		}
	}
}

func (v *validator) mcp() {
	names := map[string]bool{}
	for _, t := range v.a.Tools {
		names[t.Name] = true
	}
	servers := map[string]bool{}
	for i, s := range v.a.MCP {
		path := fmt.Sprintf("mcp[%d](%s)", i, s.Name)
		v.duration(path+".timeout", s.Timeout)
		if servers[s.Name] {
			v.add(8, path, "duplicate MCP server name")
		}
		servers[s.Name] = true
		if strings.Contains(s.Name, "__") {
			v.add(8, path+".name", "must not contain \"__\"")
		}
		snap, hasSnap := v.b.MCPSnapshots[s.Name]
		if !hasSnap {
			v.add(10, path, "%s is missing; run `tapctl mcp snapshot` and commit it", MCPSnapshotPath(s.Name))
		}
		seen := map[string]bool{}
		for j, t := range s.Tools {
			tPath := fmt.Sprintf("%s.tools[%d](%s)", path, j, t.Name)
			if seen[t.Name] {
				v.add(8, tPath, "duplicate tool")
			}
			seen[t.Name] = true
			full := MCPToolName(s.Name, t.Name)
			if names[full] {
				v.add(8, tPath, "model-visible name %q collides with another tool", full)
			}
			names[full] = true
			if hasSnap {
				if _, ok := snap[t.Name]; !ok {
					v.add(10, tPath, "not in %s; re-run `tapctl mcp snapshot`", MCPSnapshotPath(s.Name))
				}
			}
		}
		if hasSnap {
			for name := range snap {
				if !seen[name] {
					v.add(10, path, "%s pins %q which is not in the allowlist", MCPSnapshotPath(s.Name), name)
				}
			}
		}
	}
}

// Rules 3 and 9: declared secrets are exactly the used ones.
func (v *validator) secrets() {
	used := map[string]bool{}
	for i, t := range v.a.Tools {
		for j, s := range t.Secrets {
			used[s] = true
			if _, ok := v.a.Secrets[s]; !ok {
				v.add(3, fmt.Sprintf("tools[%d](%s).secrets[%d]", i, t.Name, j), "%q is not declared in secrets", s)
			}
		}
	}
	for i, s := range v.a.MCP {
		if s.Auth.Secret == "" {
			continue
		}
		used[s.Auth.Secret] = true
		if _, ok := v.a.Secrets[s.Auth.Secret]; !ok {
			v.add(9, fmt.Sprintf("mcp[%d](%s).auth.secret", i, s.Name), "%q is not declared in secrets", s.Auth.Secret)
		}
	}
	for name := range v.a.Secrets {
		if !used[name] {
			v.add(3, "secrets."+name, "declared but not used by any tool or MCP server")
		}
	}
}

// Rules 5 and 12: at least one fixture per tool; fixture args agree with the schema.
func (v *validator) fixtures() {
	schemas := map[string][]byte{}
	for _, t := range v.a.Tools {
		schemas[t.Name] = t.InputSchema
	}
	mcpTools := map[string]bool{}
	for _, s := range v.a.MCP {
		for _, t := range s.Tools {
			full := MCPToolName(s.Name, t.Name)
			mcpTools[full] = true
			if snap, ok := v.b.MCPSnapshots[s.Name]; ok && snap[t.Name] != nil {
				schemas[full] = snap[t.Name]
			}
		}
	}
	for _, t := range v.a.Tools {
		if len(v.b.Fixtures[t.Name]) == 0 {
			v.add(5, "tools."+t.Name, "no fixture; add tests/%s.test.yaml", t.Name)
		}
	}
	for name := range mcpTools {
		if len(v.b.Fixtures[name]) == 0 {
			server, tool, _ := strings.Cut(name, "__")
			v.add(12, "mcp."+name, "no fixture; add tests/mcp/%s/%s.test.yaml", server, tool)
		}
	}
	for tool, files := range v.b.Fixtures {
		raw, known := schemas[tool]
		_, isScript := v.findTool(tool)
		if !known && !mcpTools[tool] {
			for _, f := range files {
				v.add(5, f.Path, "fixture for unknown tool %q", tool)
			}
			continue
		}
		var input *jsonschema.Schema
		if raw != nil {
			if s, err := CompileSchema(tool+".input.json", raw); err == nil {
				input = s
			}
		}
		for _, f := range files {
			for i, c := range f.Fixture.Cases {
				cPath := fmt.Sprintf("%s.cases[%d](%s)", f.Path, i, c.Name)
				if mcpTools[tool] && c.MCPResponse == nil {
					v.add(12, cPath, "MCP fixtures need a recorded mcp_response")
				}
				if isScript && c.MCPResponse != nil {
					v.add(5, cPath, "mcp_response is only valid for MCP tools")
				}
				if c.Expect.OK && c.Expect.ErrorKind != "" {
					v.add(5, cPath, "expect.ok is true but error_kind is set")
				}
				if c.Expect.OutputSchema != nil {
					if _, err := CompileSchema("output.json", c.Expect.OutputSchema); err != nil {
						v.add(5, cPath+".expect.output_schema", "invalid JSON Schema: %v", err)
					}
				}
				if input == nil {
					continue
				}
				// A case with invalid args must expect the runner to reject them.
				err := ValidateValue(input, c.Args)
				wantsInvalid := c.Expect.ErrorKind == "invalid_args"
				switch {
				case err != nil && !wantsInvalid:
					v.add(5, cPath+".args", "do not match input_schema (%v); fix them or expect error_kind invalid_args", firstLine(err))
				case err == nil && wantsInvalid:
					v.add(5, cPath, "expects invalid_args but args match input_schema")
				}
			}
		}
	}
}

func (v *validator) findTool(name string) (Tool, bool) {
	i := slices.IndexFunc(v.a.Tools, func(t Tool) bool { return t.Name == name })
	if i < 0 {
		return Tool{}, false
	}
	return v.a.Tools[i], true
}

type skillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func skillFrontmatter(path string) (skillMeta, error) {
	var m skillMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return m, fmt.Errorf("missing YAML frontmatter")
	}
	var fm bytes.Buffer
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "---" {
			if err := yaml.Unmarshal(fm.Bytes(), &m); err != nil {
				return m, fmt.Errorf("frontmatter: %w", err)
			}
			return m, nil
		}
		fm.WriteString(sc.Text() + "\n")
	}
	return m, fmt.Errorf("unterminated frontmatter")
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}

func safeRel(p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	return !slices.Contains(strings.Split(p, "/"), "..")
}
