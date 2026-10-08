// Package spec loads and validates agent bundles against agent.yaml v1.
package spec

import "encoding/json"

const APIVersion = "tavon.ai/agent/v1"

type Effects string

const (
	EffectRead         Effects = "read"
	EffectWrite        Effects = "write"
	EffectIrreversible Effects = "irreversible"
)

// Rank orders effect levels so permission diffs can tell widening from narrowing.
func (e Effects) Rank() int {
	switch e {
	case EffectRead:
		return 0
	case EffectWrite:
		return 1
	case EffectIrreversible:
		return 2
	}
	return -1
}

type Agent struct {
	APIVersion    string            `json:"apiVersion"`
	Kind          string            `json:"kind"`
	Metadata      Metadata          `json:"metadata"`
	Harness       Harness           `json:"harness"`
	EffectsPolicy EffectsPolicy     `json:"effectsPolicy"`
	Prompt        string            `json:"prompt"`
	Skills        []string          `json:"skills,omitempty"`
	Runner        Runner            `json:"runner"`
	Tools         []Tool            `json:"tools,omitempty"`
	MCP           []MCPServer       `json:"mcp,omitempty"`
	Secrets       map[string]Secret `json:"secrets,omitempty"`
	Workspace     Workspace         `json:"workspace,omitempty"`
}

type Metadata struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Owner       string `json:"owner"`
	Description string `json:"description"`
}

type Harness struct {
	API      string `json:"api"`
	Model    Model  `json:"model"`
	MaxTurns int    `json:"maxTurns,omitempty"`
}

type Model struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

type EffectsPolicy struct {
	Write        string `json:"write"`
	Irreversible string `json:"irreversible"`
}

type Runner struct {
	Image     string    `json:"image"`
	Timeout   string    `json:"timeout,omitempty"`
	Resources Resources `json:"resources,omitempty"`
}

type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Exec        []string        `json:"exec"`
	Timeout     string          `json:"timeout,omitempty"`
	Secrets     []string        `json:"secrets,omitempty"`
	Egress      []string        `json:"egress,omitempty"`
	Effects     Effects         `json:"effects"`
}

type MCPServer struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Transport string    `json:"transport"`
	Auth      MCPAuth   `json:"auth"`
	Tools     []MCPTool `json:"tools"`
	Timeout   string    `json:"timeout,omitempty"`
}

type MCPAuth struct {
	Type   string `json:"type"`
	Secret string `json:"secret,omitempty"`
	Header string `json:"header,omitempty"`
}

type MCPTool struct {
	Name    string  `json:"name"`
	Effects Effects `json:"effects"`
}

type Secret struct {
	From string `json:"from"`
}

type Workspace struct {
	Size string `json:"size,omitempty"`
}

// MCPToolName is the name the model sees for an MCP tool.
func MCPToolName(server, tool string) string { return server + "__" + tool }

// Fixture is one tests/*.test.yaml file.
type Fixture struct {
	Tool  string        `json:"tool"`
	Cases []FixtureCase `json:"cases"`
}

type FixtureCase struct {
	Name        string            `json:"name"`
	Args        map[string]any    `json:"args"`
	Secrets     map[string]string `json:"secrets,omitempty"`
	HTTP        []RecordedHTTP    `json:"http,omitempty"`
	MCPResponse json.RawMessage   `json:"mcp_response,omitempty"`
	Expect      Expect            `json:"expect"`
}

type RecordedHTTP struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

type Expect struct {
	OK           bool            `json:"ok"`
	ErrorKind    string          `json:"error_kind,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}
