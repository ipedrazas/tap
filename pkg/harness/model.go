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
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Model calls one route of the AI gateway.
type Model interface {
	Complete(ctx context.Context, msgs []Message, tools []ToolDef) (Message, error)
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

func (m *GatewayModel) Complete(ctx context.Context, msgs []Message, tools []ToolDef) (Message, error) {
	body, err := json.Marshal(completionRequest{Model: m.Route, Messages: msgs, Tools: tools})
	if err != nil {
		return Message{}, err
	}
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			case <-ctx.Done():
				return Message{}, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return Message{}, err
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
			return Message{}, fmt.Errorf("model gateway: %d: %s", status, truncate(string(data), 500))
		}
		var cr chatResponse
		if err := json.Unmarshal(data, &cr); err != nil {
			return Message{}, fmt.Errorf("model gateway: %w", err)
		}
		if len(cr.Choices) == 0 {
			return Message{}, fmt.Errorf("model gateway: no choices")
		}
		return cr.Choices[0].Message, nil
	}
	return Message{}, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
