package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Trace is a session in casa's trace document format (trace_version 1):
// session -> turns -> steps. Only the fields the harness can fill are
// declared; optional numbers are pointers so "not reported" stays distinct
// from zero. See github.com/tavon-ai/casa pkg/trace.
type Trace struct {
	Version       int          `json:"trace_version"`
	ID            string       `json:"session_id"`
	Source        string       `json:"source"`
	AgentName     string       `json:"agent_name,omitempty"`
	AgentVersion  string       `json:"agent_version,omitempty"`
	PromptVersion string       `json:"prompt_version,omitempty"`
	Customer      string       `json:"customer,omitempty"`
	StartTime     time.Time    `json:"start_time,omitzero"`
	EndTime       time.Time    `json:"end_time,omitzero"`
	Turns         []*TraceTurn `json:"turns"`
}

type TraceTurn struct {
	ID        string       `json:"turn_id"`
	Index     int          `json:"turn_index"`
	Trigger   string       `json:"trigger"`
	StartTime time.Time    `json:"start_time,omitzero"`
	EndTime   time.Time    `json:"end_time,omitzero"`
	Prompt    string       `json:"prompt,omitempty"`
	Steps     []*TraceStep `json:"steps"`
}

type TraceStep struct {
	ID        string         `json:"step_id"`
	Index     int            `json:"step_index"`
	Kind      string         `json:"kind"` // llm_call | tool_call
	StartTime time.Time      `json:"start_time,omitzero"`
	EndTime   time.Time      `json:"end_time,omitzero"`
	LLMCall   *TraceLLMCall  `json:"llm_call,omitempty"`
	ToolCall  *TraceToolCall `json:"tool_call,omitempty"`
}

type TraceLLMCall struct {
	Provider      string       `json:"provider,omitempty"`
	Model         string       `json:"model,omitempty"`
	ResponseModel string       `json:"response_model,omitempty"`
	StopReason    string       `json:"stop_reason,omitempty"`
	Tokens        *TraceTokens `json:"tokens,omitempty"`
	LatencyMs     *int64       `json:"latency_ms,omitempty"`
	ContextUsed   *int64       `json:"context_used,omitempty"`
	Text          string       `json:"text,omitempty"`
	ToolCallIDs   []string     `json:"tool_call_ids,omitempty"`
}

type TraceTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

type TraceToolCall struct {
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    string          `json:"result,omitempty"`
	Status    string          `json:"status"` // ok | error | denied
	LatencyMs *int64          `json:"latency_ms,omitempty"`
}

// newTrace starts the trace of a new session.
func newTrace(id string, b *Bundle, userKey string) *Trace {
	sum := sha256.Sum256([]byte(b.SystemPrompt()))
	return &Trace{
		Version:       1,
		ID:            id,
		Source:        "tap",
		AgentName:     b.Agent.Metadata.Name,
		AgentVersion:  b.Agent.Metadata.Version,
		PromptVersion: hex.EncodeToString(sum[:8]),
		Customer:      userKey,
		Turns:         []*TraceTurn{},
	}
}

// beginTurn appends a user-triggered turn.
func (t *Trace) beginTurn(prompt string, now time.Time) *TraceTurn {
	if t.StartTime.IsZero() {
		t.StartTime = now
	}
	turn := &TraceTurn{ID: fmt.Sprintf("t%d", len(t.Turns)), Index: len(t.Turns), Trigger: "user", StartTime: now, Prompt: prompt, Steps: []*TraceStep{}}
	t.Turns = append(t.Turns, turn)
	return turn
}

// endTurn closes the turn and the session's time span.
func (t *Trace) endTurn(turn *TraceTurn, now time.Time) {
	turn.EndTime = now
	t.EndTime = now
}

// step appends a step; a nil turn (no trace being kept) records nothing.
func (turn *TraceTurn) step(kind string, start, end time.Time) *TraceStep {
	if turn == nil {
		return nil
	}
	s := &TraceStep{ID: fmt.Sprintf("%s.s%d", turn.ID, len(turn.Steps)), Index: len(turn.Steps), Kind: kind, StartTime: start, EndTime: end}
	turn.Steps = append(turn.Steps, s)
	return s
}

func (turn *TraceTurn) llmCall(route string, c Completion, start, end time.Time) *TraceStep {
	s := turn.step("llm_call", start, end)
	if s == nil {
		return nil
	}
	ms := end.Sub(start).Milliseconds()
	// casa prices calls by model, so prefer the model that answered over the
	// gateway route that picked it.
	model := c.Model
	if model == "" {
		model = route
	}
	s.LLMCall = &TraceLLMCall{
		Provider:      "gateway",
		Model:         model,
		ResponseModel: c.Model,
		StopReason:    c.FinishReason,
		LatencyMs:     &ms,
		Text:          c.Message.Content,
	}
	if u := c.Usage; u != nil {
		// OpenAI-style prompt_tokens include the cached ones; casa counts
		// input and cache reads separately.
		s.LLMCall.Tokens = &TraceTokens{
			Input:     u.PromptTokens - u.PromptTokensDetails.CachedTokens,
			Output:    u.CompletionTokens,
			CacheRead: u.PromptTokensDetails.CachedTokens,
			Reasoning: u.CompletionTokensDetails.ReasoningTokens,
		}
		used := u.PromptTokens + u.CompletionTokens
		s.LLMCall.ContextUsed = &used
	}
	return s
}

func (turn *TraceTurn) toolCall(from *TraceStep, tc ToolCall, result json.RawMessage, status string, start, end time.Time) {
	s := turn.step("tool_call", start, end)
	if s == nil {
		return
	}
	ms := end.Sub(start).Milliseconds()
	args := json.RawMessage(tc.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage("{}")
	} else if !json.Valid(args) {
		args, _ = json.Marshal(tc.Function.Arguments)
	}
	s.ToolCall = &TraceToolCall{CallID: tc.ID, Name: tc.Function.Name, Arguments: args, Result: string(result), Status: status, LatencyMs: &ms}
	if from != nil {
		from.LLMCall.ToolCallIDs = append(from.LLMCall.ToolCallIDs, s.ID)
	}
}
