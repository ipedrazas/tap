package spec

import (
	"slices"
	"strings"
	"testing"
)

const baseAcceptance = `apiVersion: tavon.ai/acceptance/v1
kind: Acceptance
criteria:
  - id: has-citations
    description: Every claim has a citation
    kind: deterministic
    exec: ["node", "checks/cited.ts"]
  - id: on-topic
    description: The answer addresses the question
    kind: judged
    rubricFile: acceptance/on-topic.md
    threshold: 0.7
    uses: [has-citations]
  - id: sample
    description: A person reads some answers
    kind: human
    sampleRate: 0.2
`

const baseCheckFixture = `check: has-citations
cases:
  - { name: cited, task: q, output: "a [1]", expect: { pass: true } }
  - { name: uncited, task: q, output: "a", expect: { pass: false, reason: "^1 claim" } }
`

func acceptanceFiles(acc string) map[string]string {
	return map[string]string{
		"acceptance.yaml":                       acc,
		"checks/cited.ts":                       "console.log('{}')",
		"acceptance/on-topic.md":                "Score 1 when it is on topic.",
		"tests/checks/has-citations.check.yaml": baseCheckFixture,
	}
}

func TestNoAcceptanceSpec(t *testing.T) {
	b := load(t, writeBundle(t, baseAgent, nil))
	if b.Agent.Acceptance != nil {
		t.Fatalf("a bundle without acceptance.yaml has a spec: %+v", b.Agent.Acceptance)
	}
	if a := Advice(b); a != nil {
		t.Fatalf("advice without a spec: %v", a)
	}
}

func TestAcceptanceValid(t *testing.T) {
	b := load(t, writeBundle(t, baseAgent, acceptanceFiles(baseAcceptance)))
	if f := Validate(b, testPlatform()); len(f) != 0 {
		t.Fatalf("expected no findings, got %v", f)
	}
	acc := b.Agent.Acceptance
	if acc.Attempts() != DefaultMaxAttempts || !acc.VerifiesOn("user") || !acc.VerifiesOn("hook") {
		t.Fatalf("defaults: attempts %d", acc.Attempts())
	}
	c, _ := acc.Find("sample")
	if c.IsBlocking() {
		t.Fatal("human criteria never block")
	}
	if d, _ := acc.Find("has-citations"); !d.IsBlocking() {
		t.Fatal("deterministic criteria block by default")
	}
	if got := b.CheckFixtures["has-citations"]; len(got) != 1 || len(got[0].Fixture.Cases) != 2 {
		t.Fatalf("check fixtures: %+v", b.CheckFixtures)
	}
}

func TestAcceptanceVerifyOn(t *testing.T) {
	acc, err := ParseAcceptance([]byte(strings.Replace(baseAcceptance, "criteria:", "verifyOn: [hook]\nmaxAttempts: 3\ncriteria:", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if acc.VerifiesOn("user") || !acc.VerifiesOn("hook") || acc.Attempts() != 3 {
		t.Fatalf("verifyOn [hook]: chat=%v hook=%v attempts=%d", acc.VerifiesOn("user"), acc.VerifiesOn("hook"), acc.Attempts())
	}
}

// Parse errors name the file and say what is wrong.
func TestAcceptanceSchemaRejects(t *testing.T) {
	tests := []struct{ name, edit, want string }{
		{"unknown kind", "kind: human\n    sampleRate: 0.2", "kind: magic\n    sampleRate: 0.2"},
		{"judged without threshold", "    threshold: 0.7\n", ""},
		{"judged with exec", "    threshold: 0.7\n", "    threshold: 0.7\n    exec: [node, checks/x.ts]\n"},
		{"judged with both rubrics", "    rubricFile: acceptance/on-topic.md\n", "    rubricFile: acceptance/on-topic.md\n    rubric: inline\n"},
		{"deterministic without exec", `    exec: ["node", "checks/cited.ts"]` + "\n", ""},
		{"human without rate", "    sampleRate: 0.2\n", ""},
		{"rate above 1", "sampleRate: 0.2", "sampleRate: 1.5"},
		{"blocking human", "    sampleRate: 0.2\n", "    sampleRate: 0.2\n    blocking: true\n"},
		{"threshold above 1", "threshold: 0.7", "threshold: 7"},
		{"bad id", "id: sample", "id: Sample_1"},
		{"unknown field", "kind: Acceptance\n", "kind: Acceptance\nretries: 3\n"},
		{"too many attempts", "kind: Acceptance\n", "kind: Acceptance\nmaxAttempts: 9\n"},
		{"bad verifyOn", "kind: Acceptance\n", "kind: Acceptance\nverifyOn: [cron]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(baseAcceptance, tc.edit, tc.want, 1)
			if src == baseAcceptance {
				t.Fatalf("edit %q did not apply", tc.edit)
			}
			_, err := ParseAcceptance([]byte(src))
			if err == nil {
				t.Fatal("parsed")
			}
			if !strings.HasPrefix(err.Error(), "acceptance.yaml: ") {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

func TestAcceptanceRules(t *testing.T) {
	tests := []struct {
		name  string
		acc   func(string) string
		extra map[string]string
		agent func(string) string
		rule  int
		want  string
	}{
		{name: "duplicate id", acc: replace("id: sample", "id: on-topic"), rule: 13, want: `duplicate id "on-topic"`},
		{name: "check script outside checks/", acc: replace("checks/cited.ts", "tools/fetch.ts"), rule: 1, want: "must be a script under checks/"},
		{name: "check script missing", acc: replace("checks/cited.ts", "checks/gone.ts"), rule: 1, want: "does not exist"},
		{name: "check through a shell", acc: replace(`["node", "checks/cited.ts"]`, `["bash", "checks/cited.ts"]`), rule: 1, want: "is a shell"},
		{name: "check with inline code", acc: replace(`["node", "checks/cited.ts"]`, `["node", "-e", "x"]`), rule: 1, want: "evaluates inline code"},
		{name: "check templated", acc: replace(`["node", "checks/cited.ts"]`, `["node", "checks/cited.ts", "{{output}}"]`), rule: 13, want: "read their input from stdin"},
		{name: "check internal egress", acc: replace(`["node", "checks/cited.ts"]`, `["node", "checks/cited.ts"]`+"\n    egress: [db.internal:443]"), rule: 4, want: "not an internal one"},
		{name: "check undeclared secret", acc: replace(`["node", "checks/cited.ts"]`, `["node", "checks/cited.ts"]`+"\n    secrets: [NOPE]"), rule: 3, want: `"NOPE" is not declared`},
		{name: "check without failing case", extra: map[string]string{"tests/checks/has-citations.check.yaml": "check: has-citations\ncases:\n  - { name: ok, task: q, output: a, expect: { pass: true } }\n"}, rule: 13, want: "1 passing, 0 failing"},
		{name: "check without fixtures", extra: map[string]string{"tests/checks/has-citations.check.yaml": ""}, rule: 13, want: "0 passing, 0 failing"},
		{name: "fixture HTTP without egress", extra: map[string]string{"tests/checks/has-citations.check.yaml": baseCheckFixture + "  - { name: web, task: q, output: a, http: [{method: GET, url: 'https://x.example.com/', status: 200}], expect: { pass: true } }\n"}, rule: 13, want: "declares no egress"},
		{name: "fixture bad reason regexp", extra: map[string]string{"tests/checks/has-citations.check.yaml": strings.Replace(baseCheckFixture, "^1 claim", "(", 1)}, rule: 13, want: "not a regular expression"},
		{name: "fixture for unknown check", extra: map[string]string{"tests/checks/other.check.yaml": "check: other\ncases:\n  - { name: x, task: q, output: a, expect: { pass: true } }\n"}, rule: 13, want: `unknown deterministic check "other"`},
		{name: "rubric outside acceptance/", acc: replace("acceptance/on-topic.md", "system.md"), rule: 13, want: "must be a file under acceptance/"},
		{name: "rubric missing", acc: replace("acceptance/on-topic.md", "acceptance/gone.md"), rule: 13, want: "does not exist"},
		{name: "uses a judged criterion", acc: replace("uses: [has-citations]", "uses: [on-topic]"), rule: 13, want: `"on-topic" is not a deterministic check`},
		{name: "unknown verifier route", acc: replace("criteria:", "verifier: { model: gpt-9 }\ncriteria:"), rule: 13, want: `"gpt-9" is not a model gateway route`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acc := baseAcceptance
			if tc.acc != nil {
				acc = tc.acc(acc)
				if acc == baseAcceptance {
					t.Fatal("acceptance edit did not apply")
				}
			}
			agent := baseAgent
			if tc.agent != nil {
				agent = tc.agent(agent)
			}
			files := acceptanceFiles(acc)
			for k, v := range tc.extra {
				files[k] = v
			}
			b := load(t, writeBundle(t, agent, files))
			findings := Validate(b, testPlatform())
			if !slices.ContainsFunc(findings, func(f Finding) bool { return f.Rule == tc.rule && strings.Contains(f.Msg, tc.want) }) {
				t.Fatalf("want rule %d %q, got %v", tc.rule, tc.want, findings)
			}
		})
	}
}

func TestCheckFixtureWithoutSpec(t *testing.T) {
	b := load(t, writeBundle(t, baseAgent, map[string]string{"tests/checks/has-citations.check.yaml": baseCheckFixture}))
	f := Validate(b, testPlatform())
	if !slices.ContainsFunc(f, func(f Finding) bool { return f.Rule == 13 && strings.Contains(f.Msg, "has no acceptance.yaml") }) {
		t.Fatalf("got %v", f)
	}
}

// Option (a): *:443 for read tools and checks, nothing else.
func TestAnyHostEgress(t *testing.T) {
	read := replace("egress: [api.example.com:443]", "egress: [\"*:443\"]")
	b := load(t, writeBundle(t, read(baseAgent), nil))
	if f := Validate(b, testPlatform()); len(f) != 0 {
		t.Fatalf("*:443 on a read tool: %v", f)
	}
	write := func(s string) string { return strings.Replace(read(s), "effects: read", "effects: write", 1) }
	b = load(t, writeBundle(t, write(baseAgent), nil))
	if f := Validate(b, testPlatform()); !slices.ContainsFunc(f, func(f Finding) bool {
		return f.Rule == 4 && strings.Contains(f.Msg, "only allowed on read tools")
	}) {
		t.Fatalf("*:443 on a write tool: %v", f)
	}
	b = load(t, writeBundle(t, replace("egress: [api.example.com:443]", `egress: ["*:80"]`)(baseAgent), nil))
	if f := Validate(b, testPlatform()); !slices.ContainsFunc(f, func(f Finding) bool { return f.Rule == 4 }) {
		t.Errorf("*:80 passed rule 4: %v", f)
	}
	// The schema already refuses partial wildcards.
	if _, err := Load(writeBundle(t, replace("egress: [api.example.com:443]", `egress: ["*.example.com:443"]`)(baseAgent), nil)); err == nil {
		t.Error("*.example.com:443 loaded")
	}
	check := replace(`["node", "checks/cited.ts"]`, `["node", "checks/cited.ts"]`+"\n    egress: [\"*:443\"]")
	b = load(t, writeBundle(t, baseAgent, acceptanceFiles(check(baseAcceptance))))
	if f := Validate(b, testPlatform()); len(f) != 0 {
		t.Fatalf("*:443 on a check: %v", f)
	}
}

func TestAcceptanceAdvice(t *testing.T) {
	writer := func(s string) string {
		s = strings.Replace(s, "effects: read", "effects: write", 1)
		return strings.Replace(s, "effectsPolicy: { write: deny", "effectsPolicy: { write: auto", 1)
	}
	b := load(t, writeBundle(t, writer(baseAgent), acceptanceFiles(baseAcceptance)))
	if f := Validate(b, testPlatform()); len(f) != 0 {
		t.Fatalf("an acting agent may have a spec: %v", f)
	}
	advice := strings.Join(Advice(b), "\n")
	for _, want := range []string{"fetch_thing run without approval", "cannot undo", "judged criteria use the agent's own model"} {
		if !strings.Contains(advice, want) {
			t.Errorf("advice lacks %q:\n%s", want, advice)
		}
	}
}

func TestAcceptanceDiff(t *testing.T) {
	head := load(t, writeBundle(t, baseAgent, acceptanceFiles(strings.Replace(baseAcceptance, `["node", "checks/cited.ts"]`, `["node", "checks/cited.ts"]`+"\n    egress: [\"*:443\"]", 1))))
	base := load(t, writeBundle(t, baseAgent, nil))
	d := Diff(base, head)
	var got []string
	for _, c := range d.Changes {
		got = append(got, c.String())
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"+ acceptance check:has-citations", "+ egress *:443 on check:has-citations", "~ criterion on-topic kind=judged"} {
		if !strings.Contains(all, want) {
			t.Errorf("diff lacks %q:\n%s", want, all)
		}
	}
	if !d.Widens() {
		t.Fatal("a new check with egress widens")
	}
}
