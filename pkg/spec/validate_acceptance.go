package spec

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Rule 13: the acceptance spec is coherent. Ids are unique; deterministic
// checks follow the tool rules (1, 3, 4) with scripts under checks/ and
// have fixtures that both pass and fail; judged criteria have a rubric and
// use only deterministic checks' evidence; the verifier model is a route.
func (v *validator) acceptance(runner *RunnerImage) {
	acc := v.a.Acceptance
	if acc == nil {
		for id, files := range v.b.CheckFixtures {
			for _, f := range files {
				v.add(13, f.Path, "fixture for check %q, but the bundle has no %s", id, AcceptanceFile)
			}
		}
		return
	}
	if v.p != nil && acc.Verifier.Model != "" && !slices.Contains(v.p.Model.Routes, acc.Verifier.Model) {
		v.add(13, "acceptance.verifier.model", "%q is not a model gateway route (have %v)", acc.Verifier.Model, v.p.Model.Routes)
	}
	seen := map[string]bool{}
	for i, c := range acc.Criteria {
		path := fmt.Sprintf("acceptance.criteria[%d](%s)", i, c.ID)
		if seen[c.ID] {
			v.add(13, path+".id", "duplicate id %q", c.ID)
		}
		seen[c.ID] = true
		switch c.Kind {
		case KindDeterministic:
			v.check(path, c, runner)
		case KindJudged:
			v.judged(path, c, acc)
		}
	}
	for id, files := range v.b.CheckFixtures {
		if c, ok := acc.Find(id); !ok || c.Kind != KindDeterministic {
			for _, f := range files {
				v.add(13, f.Path, "fixture for unknown deterministic check %q", id)
			}
		}
	}
}

func (v *validator) check(path string, c Criterion, runner *RunnerImage) {
	v.execRule(path, c.Exec, runner, ChecksDir+"/")
	for i, arg := range c.Exec {
		// A check gets the answer on stdin; nothing is substituted into argv.
		if strings.Contains(arg, "{{") || strings.Contains(arg, "}}") {
			v.add(13, fmt.Sprintf("%s.exec[%d]", path, i), "checks read their input from stdin; exec takes no {{templates}}")
		}
	}
	v.duration(path+".timeout", c.Timeout)
	v.egressRule(path, c.Egress, true)

	// A check that has never been seen to fail catches nothing.
	files := v.b.CheckFixtures[c.ID]
	var passes, fails int
	for _, f := range files {
		for j, fc := range f.Fixture.Cases {
			cPath := fmt.Sprintf("%s.cases[%d](%s)", f.Path, j, fc.Name)
			if fc.Expect.Pass {
				passes++
			} else {
				fails++
			}
			if len(fc.HTTP) > 0 && len(c.Egress) == 0 {
				v.add(13, cPath, "records HTTP responses but check %s declares no egress", c.ID)
			}
			if fc.Expect.Reason != "" {
				if _, err := regexp.Compile(fc.Expect.Reason); err != nil {
					v.add(13, cPath+".expect.reason", "not a regular expression: %v", err)
				}
			}
			for name := range fc.Secrets {
				if !slices.Contains(c.Secrets, name) {
					v.add(13, cPath+".secrets", "%s is not a secret of check %s", name, c.ID)
				}
			}
		}
	}
	if passes == 0 || fails == 0 {
		v.add(13, path, "needs fixtures in tests/checks/%s%s with at least one passing and one failing case (have %d passing, %d failing)", c.ID, CheckFixtureSuffix, passes, fails)
	}
}

func (v *validator) judged(path string, c Criterion, acc *Acceptance) {
	if c.RubricFile != "" {
		switch {
		case !safeRel(c.RubricFile) || !strings.HasPrefix(c.RubricFile, RubricsDir+"/"):
			v.add(13, path+".rubricFile", "%q must be a file under %s/", c.RubricFile, RubricsDir)
		default:
			if err := v.file(c.RubricFile); err != nil {
				v.add(13, path+".rubricFile", "%v", err)
			}
		}
	}
	for j, u := range c.Uses {
		if d, ok := acc.Find(u); !ok || d.Kind != KindDeterministic {
			v.add(13, fmt.Sprintf("%s.uses[%d]", path, j), "%q is not a deterministic check in this spec", u)
		}
	}
}

// Advice is what a reviewer should know about a bundle that is valid: the
// limits of what its acceptance spec can promise. tapctl validate prints it
// without failing.
func Advice(b *Bundle) []string {
	a := b.Agent
	acc := a.Acceptance
	if acc == nil {
		return nil
	}
	var out []string
	if a.EffectsPolicy.Write == "auto" || a.EffectsPolicy.Irreversible == "auto" {
		var acting []string
		for _, t := range a.Tools {
			if t.Effects != EffectRead && a.policy(t.Effects) == "auto" {
				acting = append(acting, t.Name)
			}
		}
		for _, s := range a.MCP {
			for _, t := range s.Tools {
				if t.Effects != EffectRead && a.policy(t.Effects) == "auto" {
					acting = append(acting, MCPToolName(s.Name, t.Name))
				}
			}
		}
		if len(acting) > 0 {
			out = append(out, fmt.Sprintf("%s: %s run without approval during the turn, before verification sees the answer; "+
				"a failed check retries the turn but cannot undo what they did. Write criteria that check the action happened as intended "+
				"(e.g. the PR exists and has the right content), and keep the agent's actions idempotent so a retry does not repeat them (docs/acceptance.md)",
				AcceptanceFile, strings.Join(acting, ", ")))
		}
	}
	judged := slices.ContainsFunc(acc.Criteria, func(c Criterion) bool { return c.Kind == KindJudged })
	if judged && (acc.Verifier.Model == "" || acc.Verifier.Model == a.Harness.Model.Name) {
		out = append(out, fmt.Sprintf("%s: judged criteria use the agent's own model route (%s); a different verifier.model is a more independent judge", AcceptanceFile, a.Harness.Model.Name))
	}
	return out
}

func (a *Agent) policy(e Effects) string {
	switch e {
	case EffectWrite:
		return a.EffectsPolicy.Write
	case EffectIrreversible:
		return a.EffectsPolicy.Irreversible
	}
	return "auto"
}
