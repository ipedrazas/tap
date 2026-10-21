package spec

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// Change is one permission-relevant difference between two versions.
type Change struct {
	// Widens is true when the change grants more than the base version had.
	Widens bool
	// Note marks a change that grants nothing new but changes what runs
	// (argv, schemas, timeouts, code): reviewers should still see it.
	Note bool
	Msg  string
}

func (c Change) String() string {
	switch {
	case c.Widens:
		return "+ " + c.Msg
	case c.Note:
		return "~ " + c.Msg
	}
	return "- " + c.Msg
}

// DiffResult holds what rules 7 and 11 need: the changes, and whether the
// version was bumped when the bundle changed.
type DiffResult struct {
	Changes []Change
	// VersionUnchanged is set when agent.yaml or another bundle file differs
	// but metadata.version does not.
	VersionUnchanged bool
	// CodeChanged is set when a bundle file other than agent.yaml was
	// added, changed or removed (DiffFiles).
	CodeChanged bool
}

func (d DiffResult) Widens() bool {
	return slices.ContainsFunc(d.Changes, func(c Change) bool { return c.Widens })
}

// permissions flattens everything that grants access, keyed for comparison.
type permissions struct {
	tools   map[string]Effects  // model-visible tool name -> effects
	secrets map[string][]string // tool or mcp server -> secrets
	egress  map[string][]string // tool or mcp server -> host:port
	servers map[string]string   // mcp server -> url
	auth    map[string]string   // mcp server -> auth type and header
	from    map[string]string   // secret -> where it is read from
	policy  EffectsPolicy
	runner  string
	owner   string // console authority and run history
	model   string
	// notes: what runs, keyed by what it belongs to.
	notes map[string]string
}

func permissionsOf(a *Agent) permissions {
	p := permissions{
		tools:   map[string]Effects{},
		secrets: map[string][]string{},
		egress:  map[string][]string{},
		servers: map[string]string{},
		auth:    map[string]string{},
		from:    map[string]string{},
		policy:  a.EffectsPolicy,
		runner:  a.Runner.Image,
		owner:   a.Metadata.Owner,
		model:   a.Harness.Model.Name,
		notes: map[string]string{
			"runner timeout":   a.Runner.Timeout,
			"runner resources": fmt.Sprintf("cpu=%s memory=%s", a.Runner.Resources.CPU, a.Runner.Resources.Memory),
			"workspace size":   a.Workspace.Size,
			"harness maxTurns": fmt.Sprint(a.Harness.MaxTurns),
			"prompt":           a.Prompt,
			"skills":           strings.Join(a.Skills, " "),
		},
	}
	for name, s := range a.Secrets {
		p.from[name] = s.From
	}
	for _, t := range a.Tools {
		p.tools[t.Name] = t.Effects
		p.secrets[t.Name] = t.Secrets
		p.egress[t.Name] = t.Egress
		p.notes["tool "+t.Name+" exec"] = strings.Join(t.Exec, " ")
		p.notes["tool "+t.Name+" input_schema"] = string(canonical(t.InputSchema))
		p.notes["tool "+t.Name+" timeout"] = t.Timeout
	}
	for _, s := range a.MCP {
		p.servers[s.Name] = s.URL
		p.auth[s.Name] = fmt.Sprintf("type=%s header=%s", s.Auth.Type, s.Auth.Header)
		p.notes["mcp server "+s.Name+" timeout"] = s.Timeout
		key := "mcp:" + s.Name
		if s.Auth.Secret != "" {
			p.secrets[key] = []string{s.Auth.Secret}
		}
		if hp, err := MCPEgress(s.URL); err == nil {
			p.egress[key] = []string{hp}
		}
		for _, t := range s.Tools {
			p.tools[MCPToolName(s.Name, t.Name)] = t.Effects
		}
	}
	return p
}

var policyRank = map[string]int{"deny": 0, "ask": 1, "auto": 2}

// Diff compares head against base. base may be nil for a new agent, in which
// case every permission counts as widening.
func Diff(base, head *Bundle) DiffResult {
	var res DiffResult
	h := permissionsOf(head.Agent)
	var b permissions
	if base != nil {
		b = permissionsOf(base.Agent)
		if !bytes.Equal(canonical(base.Raw), canonical(head.Raw)) && base.Agent.Metadata.Version == head.Agent.Metadata.Version {
			res.VersionUnchanged = true
		}
	} else {
		b = permissions{policy: EffectsPolicy{Write: "deny", Irreversible: "deny"}}
	}
	add := func(widens bool, format string, args ...any) {
		res.Changes = append(res.Changes, Change{Widens: widens, Msg: fmt.Sprintf(format, args...)})
	}
	note := func(format string, args ...any) {
		res.Changes = append(res.Changes, Change{Note: true, Msg: fmt.Sprintf(format, args...)})
	}
	// Who owns the agent and which model it talks to: a new agent states
	// them, a change widens (console authority, run history, model spend).
	if base == nil {
		add(true, "owner %s", h.owner)
		add(true, "model %s", h.model)
	} else {
		if h.owner != b.owner {
			add(true, "owner %s -> %s", b.owner, h.owner)
		}
		if h.model != b.model {
			add(true, "model %s -> %s", b.model, h.model)
		}
	}

	for _, name := range sortedKeys(h.tools, b.tools) {
		he, hok := h.tools[name]
		be, bok := b.tools[name]
		switch {
		case hok && !bok:
			add(true, "tool %s (effects: %s)", name, he)
		case !hok && bok:
			add(false, "tool %s", name)
		case he.Rank() > be.Rank():
			add(true, "tool %s effects %s -> %s", name, be, he)
		case he.Rank() < be.Rank():
			add(false, "tool %s effects %s -> %s", name, be, he)
		}
	}
	for _, name := range sortedKeys(h.servers, b.servers) {
		hu, hok := h.servers[name]
		bu, bok := b.servers[name]
		switch {
		case hok && !bok:
			add(true, "mcp server %s (%s)", name, hu)
		case !hok && bok:
			add(false, "mcp server %s", name)
		case hu != bu:
			add(true, "mcp server %s url %s -> %s", name, bu, hu)
		}
		if hok && bok && h.auth[name] != b.auth[name] {
			add(true, "mcp server %s auth %s -> %s", name, b.auth[name], h.auth[name])
		}
	}
	// The same secret name read from somewhere else (readonly-token ->
	// admin-token) hands the tool a different credential.
	for _, name := range sortedKeys(h.from, b.from) {
		if hf, bf := h.from[name], b.from[name]; hf != "" && bf != "" && hf != bf {
			add(true, "secret %s from %s -> %s", name, bf, hf)
		}
	}
	diffSets(add, "secret", h.secrets, b.secrets)
	diffSets(add, "egress", h.egress, b.egress)

	for _, lvl := range []struct {
		name   string
		hv, bv string
	}{
		{"write", h.policy.Write, b.policy.Write},
		{"irreversible", h.policy.Irreversible, b.policy.Irreversible},
	} {
		if lvl.hv != lvl.bv {
			add(policyRank[lvl.hv] > policyRank[lvl.bv], "effectsPolicy.%s %s -> %s", lvl.name, lvl.bv, lvl.hv)
		}
	}
	if base != nil {
		// Another runner image is other code running every tool.
		if h.runner != b.runner {
			add(true, "runner image %s -> %s", b.runner, h.runner)
		}
		for _, k := range sortedKeys(h.notes, b.notes) {
			hv, hok := h.notes[k]
			bv, bok := b.notes[k]
			// A tool added or removed is reported above.
			if hok && bok && hv != bv {
				note("%s %s -> %s", k, orNone(bv), orNone(hv))
			}
		}
	}
	return res
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// DiffFiles adds the bundle's other files to a diff: path -> content hash
// for everything in the agent directory except agent.yaml. Code grants no
// permission, but it changes what runs, so each change is reported, sets
// CodeChanged, and needs metadata.version bumped like agent.yaml.
func (d *DiffResult) DiffFiles(base, head *Bundle, baseFiles, headFiles map[string]string) {
	var changed []string
	for _, f := range sortedKeys(headFiles, baseFiles) {
		h, hok := headFiles[f]
		b, bok := baseFiles[f]
		switch {
		case hok && !bok:
			changed = append(changed, "file "+f+" added")
		case !hok && bok:
			changed = append(changed, "file "+f+" removed")
		case h != b:
			changed = append(changed, "file "+f+" changed")
		}
	}
	if len(changed) == 0 {
		return
	}
	d.CodeChanged = true
	const shown = 20
	for i, c := range changed {
		if i == shown {
			d.Changes = append(d.Changes, Change{Note: true, Msg: fmt.Sprintf("and %d more files", len(changed)-shown)})
			break
		}
		d.Changes = append(d.Changes, Change{Note: true, Msg: c})
	}
	if base.Agent.Metadata.Version == head.Agent.Metadata.Version {
		d.VersionUnchanged = true
	}
}

func diffSets(add func(bool, string, ...any), kind string, head, base map[string][]string) {
	for _, owner := range sortedKeys(head, base) {
		for _, v := range head[owner] {
			if !slices.Contains(base[owner], v) {
				add(true, "%s %s on %s", kind, v, owner)
			}
		}
		for _, v := range base[owner] {
			if !slices.Contains(head[owner], v) {
				add(false, "%s %s on %s", kind, v, owner)
			}
		}
	}
}

func sortedKeys[V any](a, b map[string]V) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	keys := slices.Collect(maps.Keys(set))
	sort.Strings(keys)
	return keys
}

// canonical strips formatting differences so a reformatted agent.yaml does
// not count as a change. Raw is already JSON from YAMLToJSON; re-marshalling
// via a generic value sorts keys.
func canonical(raw []byte) []byte {
	v, err := decodeAny(raw)
	if err != nil {
		return raw
	}
	out, err := marshalSorted(v)
	if err != nil {
		return raw
	}
	return out
}
