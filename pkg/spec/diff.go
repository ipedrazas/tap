package spec

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"sort"
)

// Change is one permission-relevant difference between two versions.
type Change struct {
	// Widens is true when the change grants more than the base version had.
	Widens bool
	Msg    string
}

func (c Change) String() string {
	if c.Widens {
		return "+ " + c.Msg
	}
	return "- " + c.Msg
}

// DiffResult holds what rules 7 and 11 need: the changes, and whether the
// version was bumped when agent.yaml changed.
type DiffResult struct {
	Changes []Change
	// VersionUnchanged is set when agent.yaml differs but metadata.version does not.
	VersionUnchanged bool
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
	policy  EffectsPolicy
	runner  string
}

func permissionsOf(a *Agent) permissions {
	p := permissions{
		tools:   map[string]Effects{},
		secrets: map[string][]string{},
		egress:  map[string][]string{},
		servers: map[string]string{},
		policy:  a.EffectsPolicy,
		runner:  a.Runner.Image,
	}
	for _, t := range a.Tools {
		p.tools[t.Name] = t.Effects
		p.secrets[t.Name] = t.Secrets
		p.egress[t.Name] = t.Egress
	}
	for _, s := range a.MCP {
		p.servers[s.Name] = s.URL
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
	if base != nil && h.runner != b.runner {
		// A runner bump is not a permission change, but reviewers should see it.
		add(false, "runner image %s -> %s", b.runner, h.runner)
	}
	return res
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
