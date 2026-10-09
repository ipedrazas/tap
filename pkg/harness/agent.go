// Package harness runs the agent loop. It talks only to the model gateway and
// the runner; it never executes tools itself.
package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ipedrazas/tap/pkg/spec"
)

const readSkillFile = "read_skill_file"

// Bundle is the harness projection: agent.yaml, the prompt, skills/, mcp/.
type Bundle struct {
	Dir    string
	Agent  *spec.Agent
	Prompt string
	Skills []Skill
	// Tools is what the model sees, including built-ins.
	Tools []ToolDef
	// Effects maps a model-visible tool name to its effect level.
	Effects map[string]spec.Effects
}

type Skill struct {
	Name, Description, Dir string
}

func LoadBundle(dir string) (*Bundle, error) {
	data, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		return nil, err
	}
	a, _, err := spec.ParseAgent(data)
	if err != nil {
		return nil, err
	}
	if a.Harness.API != "v1" {
		return nil, fmt.Errorf("harness supports api v1, bundle wants %q", a.Harness.API)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, a.Prompt))
	if err != nil {
		return nil, err
	}
	b := &Bundle{Dir: dir, Agent: a, Prompt: string(prompt), Effects: map[string]spec.Effects{}}
	for _, s := range a.Skills {
		meta, err := frontmatter(filepath.Join(dir, s, "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
		b.Skills = append(b.Skills, Skill{Name: meta.Name, Description: meta.Description, Dir: s})
	}
	for _, t := range a.Tools {
		b.Tools = append(b.Tools, ToolDef{Type: "function", Function: FunctionDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}})
		b.Effects[t.Name] = t.Effects
	}
	for _, s := range a.MCP {
		snapData, err := os.ReadFile(filepath.Join(dir, spec.MCPSnapshotPath(s.Name)))
		if err != nil {
			return nil, err
		}
		var snap map[string]struct {
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if err := json.Unmarshal(snapData, &snap); err != nil {
			return nil, fmt.Errorf("%s: %w", spec.MCPSnapshotPath(s.Name), err)
		}
		for _, t := range s.Tools {
			full := spec.MCPToolName(s.Name, t.Name)
			pinned := snap[t.Name]
			b.Tools = append(b.Tools, ToolDef{Type: "function", Function: FunctionDef{Name: full, Description: pinned.Description, Parameters: pinned.InputSchema}})
			b.Effects[full] = t.Effects
		}
	}
	if len(b.Skills) > 0 {
		b.Tools = append(b.Tools, ToolDef{Type: "function", Function: FunctionDef{
			Name:        readSkillFile,
			Description: "Read a file from one of your skills, e.g. skills/billing/reference.md",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
		}})
		b.Effects[readSkillFile] = spec.EffectRead
	}
	return b, nil
}

// SystemPrompt is system.md followed by the skills index (progressive
// disclosure: only names and descriptions until the model reads a file).
func (b *Bundle) SystemPrompt() string {
	if len(b.Skills) == 0 {
		return b.Prompt
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(b.Prompt))
	sb.WriteString("\n\n# Skills\n\nYou have these skills. Read a skill's SKILL.md with `read_skill_file` before relying on it.\n\n")
	for _, s := range b.Skills {
		fmt.Fprintf(&sb, "- %s (%s/SKILL.md): %s\n", s.Name, s.Dir, s.Description)
	}
	return sb.String()
}

// readSkill serves read_skill_file, confined to declared skill directories.
func (b *Bundle) readSkill(rel string) (json.RawMessage, error) {
	clean := path.Clean(rel)
	ok := slices.ContainsFunc(b.Skills, func(s Skill) bool { return strings.HasPrefix(clean, s.Dir+"/") })
	if !ok || strings.Contains(clean, "..") {
		return nil, fmt.Errorf("%q is not inside a skill directory", rel)
	}
	data, err := os.ReadFile(filepath.Join(b.Dir, clean))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", clean)
	}
	if len(data) > 256<<10 {
		data = data[:256<<10]
	}
	return json.Marshal(map[string]string{"path": clean, "content": string(data)})
}

type skillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func frontmatter(p string) (skillMeta, error) {
	var m skillMeta
	data, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return m, fmt.Errorf("missing frontmatter")
	}
	var fm bytes.Buffer
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "---" {
			return m, yaml.Unmarshal(fm.Bytes(), &m)
		}
		fm.WriteString(sc.Text() + "\n")
	}
	return m, fmt.Errorf("unterminated frontmatter")
}

// Event is streamed to the client while a turn runs.
type Event struct {
	Type    string          `json:"type"` // tool_call | tool_result | message | error
	Tool    string          `json:"tool,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Content string          `json:"content,omitempty"`
}

type Agent struct {
	Bundle *Bundle
	Model  Model
	Runner *RunnerClient
	Logger *slog.Logger
}

// Turn appends the user message, runs the loop until the model answers
// without tool calls (or maxTurns), and returns the updated history. When rec
// is not nil, each model and tool call is recorded in it.
func (a *Agent) Turn(ctx context.Context, history []Message, user string, rec *TraceTurn, emit func(Event)) ([]Message, error) {
	if len(history) == 0 {
		history = []Message{{Role: "system", Content: a.Bundle.SystemPrompt()}}
	}
	history = append(history, Message{Role: "user", Content: user})
	maxTurns := a.Bundle.Agent.Harness.MaxTurns
	if maxTurns == 0 {
		maxTurns = 40
	}
	for range maxTurns {
		start := time.Now()
		c, err := a.Model.Complete(ctx, history, a.Bundle.Tools)
		if err != nil {
			return history, err
		}
		step := rec.llmCall(a.Bundle.Agent.Harness.Model.Name, c, start, time.Now())
		msg := c.Message
		msg.Role = "assistant"
		history = append(history, msg)
		if len(msg.ToolCalls) == 0 {
			emit(Event{Type: "message", Content: msg.Content})
			return history, nil
		}
		if strings.TrimSpace(msg.Content) != "" {
			emit(Event{Type: "message", Content: msg.Content})
		}
		for _, tc := range msg.ToolCalls {
			start := time.Now()
			result, status := a.runTool(ctx, tc, emit)
			rec.toolCall(step, tc, result, status, start, time.Now())
			history = append(history, Message{Role: "tool", ToolCallID: tc.ID, Content: string(result)})
			ok := status == "ok"
			emit(Event{Type: "tool_result", Tool: tc.Function.Name, OK: &ok})
		}
	}
	emit(Event{Type: "error", Content: fmt.Sprintf("stopped after %d model turns", maxTurns)})
	return history, nil
}

// runTool returns the tool's result and its status: ok, error or denied.
func (a *Agent) runTool(ctx context.Context, tc ToolCall, emit func(Event)) (json.RawMessage, string) {
	name := tc.Function.Name
	raw := json.RawMessage(tc.Function.Arguments)
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	emit(Event{Type: "tool_call", Tool: name, Args: raw})
	errResult := func(kind, msg string) (json.RawMessage, string) {
		out, _ := json.Marshal(map[string]any{"error": map[string]string{"kind": kind, "message": msg}})
		if kind == "denied" {
			return out, "denied"
		}
		return out, "error"
	}
	effects, known := a.Bundle.Effects[name]
	if !known {
		return errResult("denied", "unknown tool "+name)
	}
	if decision := a.policy(effects); decision != "auto" {
		a.Logger.Info("tool blocked by effects policy", "tool", name, "effects", effects, "policy", decision)
		return errResult("denied", fmt.Sprintf("%s tools are %s by policy for this agent", effects, map[string]string{"deny": "denied", "ask": "awaiting human approval, which is not available yet"}[decision]))
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid_args", "arguments are not a JSON object")
	}
	if name == readSkillFile {
		p, _ := args["path"].(string)
		out, err := a.Bundle.readSkill(p)
		if err != nil {
			return errResult("invalid_args", err.Error())
		}
		return out, "ok"
	}
	resp, err := a.Runner.Call(ctx, name, args, tc.ID)
	if err != nil {
		a.Logger.Error("runner call failed", "tool", name, "err", err)
		return errResult("upstream", "the tool runner is unavailable")
	}
	if !resp.OK {
		return errResult(resp.Error.Kind, resp.Error.Message)
	}
	return resp.Output, "ok"
}

func (a *Agent) policy(e spec.Effects) string {
	switch e {
	case spec.EffectWrite:
		return a.Bundle.Agent.EffectsPolicy.Write
	case spec.EffectIrreversible:
		return a.Bundle.Agent.EffectsPolicy.Irreversible
	}
	return "auto"
}

// RunnerClient calls the runner's /v1/call on loopback.
type RunnerClient struct {
	URL    string
	Token  string
	Client *http.Client
}

type runnerResponse struct {
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output"`
	Error  *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

func (r *RunnerClient) Call(ctx context.Context, tool string, args map[string]any, callID string) (*runnerResponse, error) {
	body, _ := json.Marshal(map[string]any{"tool": tool, "args": args, "call_id": callID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/v1/call", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner: %s", resp.Status)
	}
	var rr runnerResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, err
	}
	if !rr.OK && rr.Error == nil {
		return nil, fmt.Errorf("runner: error response without error")
	}
	return &rr, nil
}

// ToolNames lists model-visible tools, sorted (for logs and /v1/info).
func (b *Bundle) ToolNames() []string {
	var names []string
	for _, t := range b.Tools {
		names = append(names, t.Function.Name)
	}
	sort.Strings(names)
	return names
}
