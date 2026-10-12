package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/ipedrazas/tap/pkg/spec"
)

// Limits on one console edit.
const (
	maxEditTools = 64
	maxEditHosts = 32
	maxUser      = 200
	maxAgentYAML = 256 << 10
)

var (
	// egressHost is stricter than rule 4: lowercase DNS names only, so an
	// entry is always a plain scalar inside a YAML flow sequence.
	egressHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*:[0-9]{1,5}$`)
	semver     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)
)

// ErrRefused marks an edit the publisher will not make (bad input, unknown
// tool, nothing to change).
var ErrRefused = errors.New("edit refused")

// EditRequest asks for a pull request that changes an existing agent's
// agent.yaml. The console sends it on behalf of User after checking that
// User may edit the agent; the publisher only ever changes tool egress lists,
// metadata.title and (always) the patch version.
type EditRequest struct {
	Agent string `json:"agent"`
	// Base is the commit the console read agent.yaml at.
	Base string `json:"base"`
	// User is the email of the person asking, for the PR body.
	User string `json:"user"`
	// Title: nil leaves metadata.title alone, "" removes it.
	Title *string `json:"title,omitempty"`
	// Egress replaces the egress list of each named tool; [] removes it.
	Egress map[string][]string `json:"egress,omitempty"`
}

// Check validates the request on its own, before agent.yaml is read.
func (r *EditRequest) Check() error {
	switch {
	case !agentName.MatchString(r.Agent):
		return fmt.Errorf("invalid agent name %q", r.Agent)
	case !commitSHA.MatchString(r.Base):
		return fmt.Errorf("base must be a full commit sha")
	case r.User == "" || len(r.User) > maxUser || strings.ContainsAny(r.User, "\r\n") || !utf8.ValidString(r.User):
		return fmt.Errorf("user must be one line of at most %d bytes", maxUser)
	case r.Title == nil && len(r.Egress) == 0:
		return fmt.Errorf("nothing to change: give a title or egress")
	case len(r.Egress) > maxEditTools:
		return fmt.Errorf("at most %d tools per edit", maxEditTools)
	}
	if r.Title != nil {
		if utf8.RuneCountInString(*r.Title) > 80 || strings.ContainsAny(*r.Title, "\r\n") || !utf8.ValidString(*r.Title) {
			return fmt.Errorf("title must be one line of at most 80 characters")
		}
	}
	for tool, hosts := range r.Egress {
		if tool == "" {
			return fmt.Errorf("empty tool name")
		}
		if len(hosts) > maxEditHosts {
			return fmt.Errorf("tool %s: at most %d egress entries", tool, maxEditHosts)
		}
		seen := map[string]bool{}
		for _, h := range hosts {
			if err := spec.CheckEgress(h); err != nil {
				return fmt.Errorf("tool %s: %w", tool, err)
			}
			if !egressHost.MatchString(h) {
				return fmt.Errorf("tool %s: %q must be a lowercase host:port", tool, h)
			}
			if seen[h] {
				return fmt.Errorf("tool %s: duplicate egress %q", tool, h)
			}
			seen[h] = true
		}
	}
	return nil
}

// Branch is the branch an edit publishes to.
func (r *EditRequest) branch(now time.Time) string {
	return fmt.Sprintf("console/%s-%d", r.Agent, now.Unix())
}

// AgentYAML returns agents/<name>/agent.yaml at the head of the base branch
// and the commit it was read at.
func (p *Publisher) AgentYAML(ctx context.Context, name string) (sha string, content []byte, err error) {
	if !agentName.MatchString(name) {
		return "", nil, fmt.Errorf("%w: invalid agent name %q", ErrRefused, name)
	}
	sha, err = p.GitHub.BranchSHA(ctx, p.Base)
	if err != nil {
		return "", nil, err
	}
	content, err = p.GitHub.File(ctx, "agents/"+name+"/agent.yaml", sha)
	return sha, content, err
}

// Edit opens a pull request that applies r to the agent's agent.yaml at
// r.Base. Only that file changes; everything but the requested fields and the
// version is kept byte for byte.
func (p *Publisher) Edit(ctx context.Context, r EditRequest) (PullRequest, error) {
	if err := r.Check(); err != nil {
		return PullRequest{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	path := "agents/" + r.Agent + "/agent.yaml"
	orig, err := p.GitHub.File(ctx, path, r.Base)
	if err != nil {
		return PullRequest{}, err
	}
	res, err := EditAgentYAML(orig, r)
	if err != nil {
		return PullRequest{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	branch := r.branch(now())
	tree, err := p.GitHub.commitTree(ctx, r.Base)
	if err != nil {
		return PullRequest{}, fmt.Errorf("base commit: %w", err)
	}
	blob, err := p.GitHub.createBlob(ctx, res.Content)
	if err != nil {
		return PullRequest{}, fmt.Errorf("blob %s: %w", path, err)
	}
	newTree, err := p.GitHub.createTree(ctx, tree, []treeEntry{{Path: path, Mode: "100644", Type: "blob", SHA: blob}})
	if err != nil {
		return PullRequest{}, fmt.Errorf("tree: %w", err)
	}
	commit, err := p.GitHub.createCommit(ctx, fmt.Sprintf("%s\n\nRequested by %s in tap-console.", res.Title(r.Agent), r.User), newTree, r.Base)
	if err != nil {
		return PullRequest{}, fmt.Errorf("commit: %w", err)
	}
	if err := p.GitHub.createRef(ctx, branch, commit); err != nil {
		return PullRequest{}, fmt.Errorf("branch %s: %w", branch, err)
	}
	pr, err := p.GitHub.createPR(ctx, res.Title(r.Agent), res.Body(r), branch, p.Base)
	if err != nil {
		return PullRequest{}, fmt.Errorf("pull request: %w", err)
	}
	if err := p.GitHub.addLabels(ctx, pr.Number, "console"); err != nil {
		p.Logger.Warn("label failed", "pr", pr.Number, "err", err)
	}
	return pr, nil
}

// EditResult is an edited agent.yaml and what changed.
type EditResult struct {
	Content                []byte
	OldVersion, NewVersion string
	// TitleChanged is set when metadata.title changed; NewTitle "" means removed.
	TitleChanged bool
	NewTitle     string
	// Added and Removed egress per tool.
	Added, Removed map[string][]string
}

// Title is the pull request title.
func (e *EditResult) Title(agent string) string {
	var what []string
	if len(e.Added)+len(e.Removed) > 0 {
		what = append(what, "egress")
	}
	if e.TitleChanged {
		what = append(what, "title")
	}
	return fmt.Sprintf("%s: update %s (%s -> %s)", agent, strings.Join(what, " and "), e.OldVersion, e.NewVersion)
}

// Body is the pull request body.
func (e *EditResult) Body(r EditRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Changes to `agents/%s/agent.yaml` requested in tap-console.\n\n", r.Agent)
	tools := map[string]bool{}
	for t := range e.Added {
		tools[t] = true
	}
	for t := range e.Removed {
		tools[t] = true
	}
	if len(tools) > 0 {
		b.WriteString("## Egress\n\n")
		for _, t := range slices.Sorted(mapKeys(tools)) {
			fmt.Fprintf(&b, "- `%s`\n", t)
			for _, h := range e.Added[t] {
				fmt.Fprintf(&b, "  - **+ %s** (widens permissions)\n", h)
			}
			for _, h := range e.Removed[t] {
				fmt.Fprintf(&b, "  - − %s\n", h)
			}
		}
		b.WriteString("\n")
		if len(e.Added) > 0 {
			b.WriteString("> [!WARNING]\n> This change widens egress (rule 7) and needs human review. Check that the tool's code actually calls the new hosts.\n\n")
		}
	}
	if e.TitleChanged {
		if e.NewTitle == "" {
			b.WriteString("## Title\n\nRemoves `metadata.title` (the console shows the name).\n\n")
		} else {
			fmt.Fprintf(&b, "## Title\n\nSets `metadata.title` to %q.\n\n", e.NewTitle)
		}
	}
	fmt.Fprintf(&b, "## Version\n\n%s → %s (bundle versions are immutable). After merging, launch it with `task agent:launch AGENT=%s`.\n\n", e.OldVersion, e.NewVersion, r.Agent)
	fmt.Fprintf(&b, "Requested by %s in tap-console.\n", r.User)
	return b.String()
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// lineEdit replaces lines [start, end] (1-based, inclusive) with lines;
// end == start-1 inserts before start.
type lineEdit struct {
	start, end int
	lines      []string
}

// EditAgentYAML applies r to agent.yaml text. It edits lines in place, so
// comments, blank lines and formatting elsewhere survive, then checks that
// the result parses, passes the schema and differs from the original in the
// requested fields and the version only.
func EditAgentYAML(orig []byte, r EditRequest) (*EditResult, error) {
	if len(orig) > maxAgentYAML {
		return nil, fmt.Errorf("agent.yaml is larger than %d bytes", maxAgentYAML)
	}
	if bytes.Contains(orig, []byte("\r")) {
		return nil, fmt.Errorf("agent.yaml has CRLF line endings")
	}
	a, _, err := spec.ParseAgent(orig)
	if err != nil {
		return nil, err
	}
	if a.Metadata.Name != r.Agent {
		return nil, fmt.Errorf("agent.yaml names %q, not %q", a.Metadata.Name, r.Agent)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(orig, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("agent.yaml is not a single mapping")
	}
	lines := strings.Split(string(orig), "\n")
	root := doc.Content[0]
	res := &EditResult{OldVersion: a.Metadata.Version, Added: map[string][]string{}, Removed: map[string][]string{}}
	var edits []lineEdit

	// What the data must look like afterwards, built on the original.
	want, err := asData(orig)
	if err != nil {
		return nil, err
	}
	wantMeta := want["metadata"].(map[string]any)

	meta := mapValue(root, "metadata")
	if meta == nil || meta.Kind != yaml.MappingNode || meta.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("metadata must be a block mapping")
	}

	// Egress.
	if len(r.Egress) > 0 {
		toolsNode := mapValue(root, "tools")
		if toolsNode == nil || toolsNode.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("agent has no tools")
		}
		wantTools, _ := want["tools"].([]any)
		for name, hosts := range r.Egress {
			i := slices.IndexFunc(a.Tools, func(t spec.Tool) bool { return t.Name == name })
			if i < 0 {
				return nil, fmt.Errorf("agent has no tool %q (MCP servers' egress comes from their url and cannot be edited)", name)
			}
			old := a.Tools[i].Egress
			for _, h := range hosts {
				if !slices.Contains(old, h) {
					res.Added[name] = append(res.Added[name], h)
				}
			}
			for _, h := range old {
				if !slices.Contains(hosts, h) {
					res.Removed[name] = append(res.Removed[name], h)
				}
			}
			if slices.Equal(old, hosts) {
				continue
			}
			tn := toolsNode.Content[i]
			if tn.Kind != yaml.MappingNode || tn.Style&yaml.FlowStyle != 0 {
				return nil, fmt.Errorf("tool %s must be a block mapping", name)
			}
			e, err := setKey(lines, tn, "egress", flowList(hosts), len(hosts) == 0, "")
			if err != nil {
				return nil, fmt.Errorf("tool %s: %w", name, err)
			}
			edits = append(edits, e...)
			wt := wantTools[i].(map[string]any)
			if len(hosts) == 0 {
				delete(wt, "egress")
			} else {
				wt["egress"] = toAny(hosts)
			}
		}
		for t := range res.Added {
			sort.Strings(res.Added[t])
		}
		for t := range res.Removed {
			sort.Strings(res.Removed[t])
		}
	}

	// Title.
	if r.Title != nil && *r.Title != a.Metadata.Title {
		res.TitleChanged, res.NewTitle = true, *r.Title
		e, err := setKey(lines, meta, "title", quote(*r.Title), *r.Title == "", "owner")
		if err != nil {
			return nil, fmt.Errorf("metadata.title: %w", err)
		}
		edits = append(edits, e...)
		if *r.Title == "" {
			delete(wantMeta, "title")
		} else {
			wantMeta["title"] = *r.Title
		}
	}

	if len(edits) == 0 {
		return nil, fmt.Errorf("nothing to change: agent.yaml already says that")
	}

	// Version.
	res.NewVersion, err = bumpPatch(a.Metadata.Version)
	if err != nil {
		return nil, err
	}
	e, err := setKey(lines, meta, "version", res.NewVersion, false, "")
	if err != nil {
		return nil, fmt.Errorf("metadata.version: %w", err)
	}
	edits = append(edits, e...)
	wantMeta["version"] = res.NewVersion

	out, err := applyEdits(lines, edits)
	if err != nil {
		return nil, err
	}
	if _, _, err := spec.ParseAgent(out); err != nil {
		return nil, fmt.Errorf("edited agent.yaml is invalid: %w", err)
	}
	got, err := asData(out)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(got, want) {
		return nil, fmt.Errorf("edited agent.yaml changes more than asked (layout not supported); edit it by hand")
	}
	res.Content = out
	return res, nil
}

func asData(b []byte) (map[string]any, error) {
	raw, err := sigsyaml.YAMLToJSON(b)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func toAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func flowList(hosts []string) string { return "[" + strings.Join(hosts, ", ") + "]" }

func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func bumpPatch(v string) (string, error) {
	m := semver.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("metadata.version %q is not semver", v)
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%s.%d", m[1], m[2], patch+1), nil
}

// mapValue returns the value node of key in a mapping.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if i := keyIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}

func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// setKey writes "key: value" in block mapping m: it replaces the existing
// entry's lines, removes them (remove), or inserts a new line after the
// entry named after (or after the last entry when after is "" or absent).
func setKey(lines []string, m *yaml.Node, key, value string, remove bool, after string) ([]lineEdit, error) {
	if len(m.Content) == 0 {
		return nil, fmt.Errorf("empty mapping")
	}
	indent := strings.Repeat(" ", m.Content[0].Column-1)
	i := keyIndex(m, key)
	if i >= 0 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Column-1 != len(indent) || !strings.HasPrefix(lines[k.Line-1], indent+key+":") {
			return nil, fmt.Errorf("unsupported layout at line %d", k.Line)
		}
		end, err := entryEnd(lines, m, i)
		if err != nil {
			return nil, err
		}
		if remove {
			return []lineEdit{{start: k.Line, end: end}}, nil
		}
		line := indent + key + ": " + value
		if v.LineComment != "" && end == k.Line {
			line += " " + v.LineComment
		}
		return []lineEdit{{start: k.Line, end: end, lines: []string{line}}}, nil
	}
	if remove {
		return nil, nil
	}
	j := len(m.Content) - 2
	if after != "" {
		if a := keyIndex(m, after); a >= 0 {
			j = a
		}
	}
	end, err := entryEnd(lines, m, j)
	if err != nil {
		return nil, err
	}
	return []lineEdit{{start: end + 1, end: end, lines: []string{indent + key + ": " + value}}}, nil
}

// entryEnd is the last line of mapping entry i (its key at Content[i]).
func entryEnd(lines []string, m *yaml.Node, i int) (int, error) {
	k, v := m.Content[i], m.Content[i+1]
	end := maxLine(v)
	if k.Line > end {
		end = k.Line
	}
	if v.Style&yaml.FlowStyle != 0 && (v.Kind == yaml.SequenceNode || v.Kind == yaml.MappingNode) {
		// The closing bracket may sit on a later line than the last item.
		depth := 0
		for l := v.Line; l <= len(lines); l++ {
			text := lines[l-1]
			if l == v.Line {
				text = text[v.Column-1:]
			}
			for _, c := range text {
				switch c {
				case '[', '{':
					depth++
				case ']', '}':
					depth--
				}
			}
			if depth <= 0 {
				if l > end {
					end = l
				}
				break
			}
		}
	}
	if v.Kind == yaml.ScalarNode && (v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0) {
		// Block scalars run until the next entry.
		next := len(lines)
		if i+2 < len(m.Content) {
			next = m.Content[i+2].Line - 1
		}
		for next > end && strings.TrimSpace(lines[next-1]) == "" {
			next--
		}
		end = next
	}
	if end > len(lines) {
		return 0, fmt.Errorf("entry at line %d runs past the end of the file", k.Line)
	}
	return end, nil
}

func maxLine(n *yaml.Node) int {
	l := n.Line
	for _, c := range n.Content {
		if x := maxLine(c); x > l {
			l = x
		}
	}
	return l
}

func applyEdits(lines []string, edits []lineEdit) ([]byte, error) {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for i := 1; i < len(edits); i++ {
		if edits[i].end >= edits[i-1].start {
			return nil, fmt.Errorf("overlapping edits at line %d", edits[i].start)
		}
	}
	out := slices.Clone(lines)
	for _, e := range edits {
		out = slices.Concat(out[:e.start-1], e.lines, out[e.end:])
	}
	return []byte(strings.Join(out, "\n")), nil
}
