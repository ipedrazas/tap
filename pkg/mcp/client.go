// Package mcp is a minimal Model Context Protocol client for the Streamable
// HTTP transport: initialize, tools/list and tools/call. Responses may come
// back as application/json or as a text/event-stream.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

const ProtocolVersion = "2025-06-18"

// MaxResponse bounds any single response body.
const MaxResponse = 4 << 20

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations *Annotations    `json:"annotations,omitempty"`
}

// Annotations are the server's own hints; untrusted, but useful for review.
type Annotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
}

type Client struct {
	URL string
	// Header is added to every request (e.g. Authorization). Never logged.
	Header http.Header
	HTTP   *http.Client

	mu          sync.Mutex // serialises initialization
	initialized bool
	smu         sync.Mutex // guards session
	session     string
	nextID      atomic.Int64
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// ErrSessionExpired is returned when the server forgot our session.
var errSessionExpired = errors.New("mcp session expired")

func (c *Client) ensureInit(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "tap-runner", "version": "1"},
	}
	if _, err := c.do(ctx, "initialize", params, true); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if _, err := c.do(ctx, "notifications/initialized", nil, false); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	c.initialized = true
	return nil
}

// call runs a request, re-initializing once if the session expired.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		if err := c.ensureInit(ctx); err != nil {
			return nil, err
		}
		res, err := c.do(ctx, method, params, true)
		if errors.Is(err, errSessionExpired) && attempt == 0 {
			c.mu.Lock()
			c.initialized = false
			c.mu.Unlock()
			c.setSession("")
			continue
		}
		return res, err
	}
}

func (c *Client) do(ctx context.Context, method string, params any, wantResult bool) (json.RawMessage, error) {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	var id int64
	if wantResult {
		id = c.nextID.Add(1)
		req.ID = &id
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range c.Header {
		hr.Header[k] = v
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json, text/event-stream")
	if method != "initialize" {
		hr.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	}
	session := c.getSession()
	if session != "" {
		hr.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" && method == "initialize" {
		c.setSession(sid)
	}
	if resp.StatusCode == http.StatusNotFound && session != "" {
		return nil, errSessionExpired
	}
	if !wantResult {
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("%s: %s", method, resp.Status)
		}
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s: %s", method, resp.Status, strings.TrimSpace(string(snippet)))
	}
	r, err := readResponse(resp, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

// readResponse handles both a JSON body and an SSE stream, returning the
// JSON-RPC response whose id matches.
func readResponse(resp *http.Response, id int64) (*rpcResponse, error) {
	body := io.LimitReader(resp.Body, MaxResponse)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var r rpcResponse
		if err := json.NewDecoder(body).Decode(&r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), MaxResponse)
	var data strings.Builder
	flush := func() (*rpcResponse, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return nil, false
		}
		var r rpcResponse
		if json.Unmarshal([]byte(data.String()), &r) != nil || r.ID == nil || *r.ID != id {
			return nil, false // notifications or other messages
		}
		return &r, true
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if r, ok := flush(); ok {
				return r, nil
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(v, " "))
		}
	}
	if r, ok := flush(); ok {
		return r, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("stream ended without a response")
}

// ListTools returns every tool the server offers, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for range 50 {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("tools/list: too many pages")
}

// CallTool returns the raw CallToolResult.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	return c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
}

func (c *Client) getSession() string {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.session
}

func (c *Client) setSession(s string) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.session = s
}
