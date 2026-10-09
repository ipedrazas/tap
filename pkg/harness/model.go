package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAI-compatible chat completion types, as spoken by the Envoy AI Gateway.

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type completionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

type Usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// Completion is one model response: the message plus what the trace needs.
type Completion struct {
	Message      Message
	FinishReason string
	Model        string // the model that answered, as the gateway reports it
	Usage        *Usage // nil when the gateway reports none
}

// Model calls one route of the AI gateway.
type Model interface {
	Complete(ctx context.Context, msgs []Message, tools []ToolDef) (Completion, error)
}

type GatewayModel struct {
	BaseURL   string // .../v1
	Route     string // x-ai-eg-model
	KeyHeader string
	Key       string
	Client    *http.Client
}

// retryable covers the gateway being briefly unreachable, e.g. while the
// network policy controller admits a new pod's IP.
func retryable(status int, err error) bool {
	return err != nil || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout || status == http.StatusTooManyRequests
}

func (m *GatewayModel) Complete(ctx context.Context, msgs []Message, tools []ToolDef) (Completion, error) {
	body, err := json.Marshal(completionRequest{Model: m.Route, Messages: msgs, Tools: tools})
	if err != nil {
		return Completion{}, err
	}
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			case <-ctx.Done():
				return Completion{}, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return Completion{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ai-eg-model", m.Route)
		if m.KeyHeader != "" {
			req.Header.Set(m.KeyHeader, m.Key)
		}
		resp, err := m.Client.Do(req)
		status := 0
		var data []byte
		if err == nil {
			status = resp.StatusCode
			data, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
		}
		if retryable(status, err) {
			lastErr = fmt.Errorf("model gateway: status %d: %v", status, err)
			continue
		}
		if status != http.StatusOK {
			return Completion{}, fmt.Errorf("model gateway: %d: %s", status, truncate(string(data), 500))
		}
		var cr chatResponse
		if err := json.Unmarshal(data, &cr); err != nil {
			return Completion{}, fmt.Errorf("model gateway: %w", err)
		}
		if len(cr.Choices) == 0 {
			return Completion{}, fmt.Errorf("model gateway: no choices")
		}
		return Completion{Message: cr.Choices[0].Message, FinishReason: cr.Choices[0].FinishReason, Model: cr.Model, Usage: cr.Usage}, nil
	}
	return Completion{}, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
