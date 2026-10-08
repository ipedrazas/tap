// Package scaffold writes a new, valid agent bundle that a person or the
// factory then edits. Starting from something that passes validation keeps
// every later failure about the agent's own content.
package scaffold

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/ipedrazas/tap/pkg/spec"
)

//go:embed templates
var templates embed.FS

type Options struct {
	Dir, Name, Owner, Description string
	// Runner is a curated runner name from platform.yaml: runner-node or runner-python.
	Runner string
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)

func New(opts Options, p *spec.Platform) error {
	if !nameRe.MatchString(opts.Name) {
		return fmt.Errorf("name %q must be a DNS label: lowercase letters, digits and dashes, 3-40 chars", opts.Name)
	}
	if _, err := os.Stat(opts.Dir); err == nil {
		return fmt.Errorf("%s already exists", opts.Dir)
	}
	var image string
	for _, r := range p.Runners {
		if r.Name == opts.Runner {
			image = r.Image
		}
	}
	if image == "" {
		return fmt.Errorf("runner %q has no pinned image in platform.yaml", opts.Runner)
	}
	lang, exec, script := "node", `"node", "tools/example_lookup.ts"`, "example_lookup.ts"
	switch opts.Runner {
	case "runner-node":
	case "runner-python":
		lang, exec, script = "python", `"python3", "tools/example_lookup.py"`, "example_lookup.py"
	default:
		return fmt.Errorf("scaffolds exist for runner-node and runner-python, not %q", opts.Runner)
	}
	data := map[string]string{
		"Name":        opts.Name,
		"Owner":       opts.Owner,
		"Description": strings.TrimSpace(opts.Description),
		"Skill":       strings.TrimSuffix(opts.Name, "-agent"),
		"RunnerImage": image,
		"Exec":        exec,
		// JSON strings are valid YAML scalars, so any text is safe here.
		"DescriptionYAML": jsonString(strings.TrimSpace(opts.Description)),
		"OwnerYAML":       jsonString(opts.Owner),
	}
	files := map[string]string{
		"agent.yaml":                            "templates/agent.yaml.tmpl",
		"system.md":                             "templates/system.md.tmpl",
		"skills/" + data["Skill"] + "/SKILL.md": "templates/SKILL.md.tmpl",
		"tests/example_lookup.test.yaml":        "templates/example_lookup.test.yaml.tmpl",
		"tools/" + script:                       "templates/" + lang + "/" + script,
	}
	for dst, src := range files {
		raw, err := templates.ReadFile(src)
		if err != nil {
			return err
		}
		out := raw
		if strings.HasSuffix(src, ".tmpl") {
			t, err := template.New(src).Parse(string(raw))
			if err != nil {
				return err
			}
			var sb strings.Builder
			if err := t.Execute(&sb, data); err != nil {
				return err
			}
			out = []byte(sb.String())
		}
		path := filepath.Join(opts.Dir, dst)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
