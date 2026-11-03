package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ipedrazas/tap/schema"
)

// An acceptance spec (acceptance.yaml, optional) says what a good answer
// from the agent looks like. The harness verifies each answer against it
// outside the producing conversation: deterministic checks run in the
// runner like tools, judged ones are separate model calls with no tools, and
// human ones flag a sample of runs for review. See docs/acceptance.md.

const (
	AcceptanceAPIVersion = "tavon.ai/acceptance/v1"
	AcceptanceFile       = "acceptance.yaml"
	// ChecksDir holds deterministic check scripts (runner projection).
	ChecksDir = "checks"
	// RubricsDir holds judged criteria's rubric files (harness projection).
	RubricsDir = "acceptance"

	KindDeterministic = "deterministic"
	KindJudged        = "judged"
	KindHuman         = "human"

	DefaultMaxAttempts = 2
)

type Acceptance struct {
	APIVersion  string      `json:"apiVersion"`
	Kind        string      `json:"kind"`
	MaxAttempts int         `json:"maxAttempts,omitempty"`
	VerifyOn    []string    `json:"verifyOn,omitempty"`
	Verifier    Verifier    `json:"verifier,omitzero"`
	Criteria    []Criterion `json:"criteria"`
	// Raw is acceptance.yaml converted to JSON (diffs).
	Raw []byte `json:"-"`
}

type Verifier struct {
	Model string `json:"model,omitempty"`
}

type Criterion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Blocking    *bool  `json:"blocking,omitempty"`
	// deterministic
	Exec    []string `json:"exec,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
	Egress  []string `json:"egress,omitempty"`
	Secrets []string `json:"secrets,omitempty"`
	// judged
	Rubric     string   `json:"rubric,omitempty"`
	RubricFile string   `json:"rubricFile,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	Uses       []string `json:"uses,omitempty"`
	// human
	SampleRate float64 `json:"sampleRate,omitempty"`
}

// IsBlocking reports whether a failure of c keeps the answer from being done.
func (c Criterion) IsBlocking() bool {
	if c.Kind == KindHuman {
		return false
	}
	return c.Blocking == nil || *c.Blocking
}

// CheckScope is the egress scope (and runner name) of a deterministic check.
// Tool names cannot contain ':', so no tool can mint a check's credential.
func CheckScope(id string) string { return "check:" + id }

// Attempts is how many times the agent may answer, the first included.
func (a *Acceptance) Attempts() int {
	if a == nil || a.MaxAttempts == 0 {
		return DefaultMaxAttempts
	}
	return a.MaxAttempts
}

// VerifiesOn reports whether turns started by trigger (chat or hook) are verified.
func (a *Acceptance) VerifiesOn(trigger string) bool {
	if a == nil {
		return false
	}
	if trigger == "user" || trigger == "local" {
		trigger = "chat"
	}
	if len(a.VerifyOn) == 0 {
		return true
	}
	return slices.Contains(a.VerifyOn, trigger)
}

// Deterministic returns the deterministic criteria, in spec order.
func (a *Acceptance) Deterministic() []Criterion {
	if a == nil {
		return nil
	}
	var out []Criterion
	for _, c := range a.Criteria {
		if c.Kind == KindDeterministic {
			out = append(out, c)
		}
	}
	return out
}

// Find returns the criterion with id.
func (a *Acceptance) Find(id string) (Criterion, bool) {
	if a != nil {
		for _, c := range a.Criteria {
			if c.ID == id {
				return c, true
			}
		}
	}
	return Criterion{}, false
}

var (
	acceptanceSchema   = mustCompile("acceptance.v1.json", schema.AcceptanceV1)
	checkFixtureSchema = mustCompile("check-fixture.v1.json", schema.CheckFixtureV1)
)

// ParseAcceptance schema-validates acceptance.yaml bytes and decodes them.
// Errors name the file and the failing path, so they read on their own.
func ParseAcceptance(b []byte) (*Acceptance, error) {
	raw, err := yaml.YAMLToJSON(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", AcceptanceFile, err)
	}
	if err := validateJSON(acceptanceSchema, raw); err != nil {
		return nil, fmt.Errorf("%s: %w", AcceptanceFile, err)
	}
	var a Acceptance
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("%s: %w", AcceptanceFile, err)
	}
	a.Raw = raw
	return &a, nil
}

// LoadAcceptance reads dir/acceptance.yaml. A bundle without one has no
// acceptance spec: nil, nil, and the agent runs exactly as before.
func LoadAcceptance(dir string) (*Acceptance, error) {
	b, err := ReadFile(dir, AcceptanceFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseAcceptance(b)
}

// CheckFixture is one tests/checks/<id>.check.yaml file: inputs a check
// receives and whether it must pass them. Recorded HTTP replays through the
// mock egress proxy, as for tool fixtures.
type CheckFixture struct {
	Check string             `json:"check"`
	Cases []CheckFixtureCase `json:"cases"`
}

type CheckFixtureCase struct {
	Name    string            `json:"name"`
	Task    string            `json:"task"`
	Output  string            `json:"output"`
	Secrets map[string]string `json:"secrets,omitempty"`
	HTTP    []RecordedHTTP    `json:"http,omitempty"`
	Expect  CheckExpect       `json:"expect"`
}

type CheckExpect struct {
	Pass bool `json:"pass"`
	// Reason, when set, is a regular expression the check's reason must match.
	Reason string `json:"reason,omitempty"`
}

type CheckFixtureFile struct {
	Path    string
	Fixture CheckFixture
}

// CheckFixtureSuffix names check fixtures, so LoadFixtures (*.test.yaml)
// never reads them as tool fixtures.
const CheckFixtureSuffix = ".check.yaml"

// LoadCheckFixtures reads every *.check.yaml under root (a bundle's tests/
// dir), keyed by check id. Paths are reported as tests/<rel>.
func LoadCheckFixtures(root string) (map[string][]CheckFixtureFile, error) {
	out := map[string][]CheckFixtureFile{}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return out, nil
	}
	var paths []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, CheckFixtureSuffix) && !strings.HasPrefix(d.Name(), "._") {
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
		data, err := ReadFile(root, r)
		if err != nil {
			return nil, err
		}
		raw, err := yaml.YAMLToJSON(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if err := validateJSON(checkFixtureSchema, raw); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		var f CheckFixture
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		out[f.Check] = append(out[f.Check], CheckFixtureFile{Path: rel, Fixture: f})
	}
	return out, nil
}
