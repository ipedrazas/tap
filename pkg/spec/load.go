package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"

	"github.com/ipedrazas/tap/schema"
)

// Bundle is an agent source directory loaded from disk.
type Bundle struct {
	Dir   string
	Agent *Agent
	// Raw is agent.yaml converted to JSON, used for hashing and diffs.
	Raw []byte
	// Fixtures maps a model-visible tool name to its fixture files.
	Fixtures map[string][]FixtureFile
	// MCPSnapshots maps an MCP server name to its pinned tool schemas.
	MCPSnapshots map[string]map[string]json.RawMessage
}

type FixtureFile struct {
	Path    string
	Fixture Fixture
}

// MCPSnapshotPath is where `tapctl mcp snapshot` writes a server's pinned schemas.
func MCPSnapshotPath(server string) string { return filepath.Join("mcp", server+".tools.json") }

var (
	agentSchema   = mustCompile("agent.v1.json", schema.AgentV1)
	fixtureSchema = mustCompile("fixture.v1.json", schema.FixtureV1)
)

func mustCompile(name string, b []byte) *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		panic(fmt.Sprintf("schema %s: %v", name, err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name, doc); err != nil {
		panic(fmt.Sprintf("schema %s: %v", name, err))
	}
	s, err := c.Compile(name)
	if err != nil {
		panic(fmt.Sprintf("schema %s: %v", name, err))
	}
	return s
}

// ParseAgent schema-validates agent.yaml bytes and decodes them.
func ParseAgent(b []byte) (*Agent, []byte, error) {
	raw, err := yaml.YAMLToJSON(b)
	if err != nil {
		return nil, nil, fmt.Errorf("agent.yaml: %w", err)
	}
	if err := validateJSON(agentSchema, raw); err != nil {
		return nil, nil, fmt.Errorf("agent.yaml: %w", err)
	}
	var a Agent
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, nil, fmt.Errorf("agent.yaml: %w", err)
	}
	return &a, raw, nil
}

// Load reads agent.yaml, fixtures and MCP snapshots from dir. Schema errors are
// returned as an error; semantic rules are checked by Validate.
func Load(dir string) (*Bundle, error) {
	b, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		return nil, err
	}
	a, raw, err := ParseAgent(b)
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{
		Dir:          dir,
		Agent:        a,
		Raw:          raw,
		Fixtures:     map[string][]FixtureFile{},
		MCPSnapshots: map[string]map[string]json.RawMessage{},
	}
	if err := bundle.loadFixtures(); err != nil {
		return nil, err
	}
	if err := bundle.loadSnapshots(); err != nil {
		return nil, err
	}
	return bundle, nil
}

func (b *Bundle) loadFixtures() error {
	f, err := LoadFixtures(filepath.Join(b.Dir, "tests"))
	if err != nil {
		return err
	}
	b.Fixtures = f
	return nil
}

// LoadFixtures reads every *.test.yaml under root (a bundle's tests/ dir),
// keyed by tool name. Paths are reported as tests/<rel>.
func LoadFixtures(root string) (map[string][]FixtureFile, error) {
	out := map[string][]FixtureFile{}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return out, nil
	}
	var paths []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".test.yaml") && !strings.HasPrefix(d.Name(), "._") {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		r, _ := filepath.Rel(root, p)
		rel := filepath.ToSlash(filepath.Join("tests", r))
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		raw, err := yaml.YAMLToJSON(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if err := validateJSON(fixtureSchema, raw); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		var f Fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		out[f.Tool] = append(out[f.Tool], FixtureFile{Path: rel, Fixture: f})
	}
	return out, nil
}

func (b *Bundle) loadSnapshots() error {
	for _, s := range b.Agent.MCP {
		p := filepath.Join(b.Dir, MCPSnapshotPath(s.Name))
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue // reported by rule 10
		}
		if err != nil {
			return err
		}
		var snap map[string]json.RawMessage
		if err := json.Unmarshal(data, &snap); err != nil {
			return fmt.Errorf("%s: %w", MCPSnapshotPath(s.Name), err)
		}
		b.MCPSnapshots[s.Name] = snap
	}
	return nil
}

func validateJSON(s *jsonschema.Schema, raw []byte) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return s.Validate(v)
}

// CompileSchema compiles an inline JSON Schema (tool input or fixture output schema).
func CompileSchema(name string, raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(name, doc); err != nil {
		return nil, err
	}
	return c.Compile(name)
}

// ValidateValue validates a decoded JSON value (e.g. tool args) against s.
func ValidateValue(s *jsonschema.Schema, v any) error {
	// Round-trip through JSON so numbers are json.Number-compatible for the validator.
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return validateJSON(s, raw)
}
