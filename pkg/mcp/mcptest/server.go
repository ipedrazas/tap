// Package mcptest is a fake Streamable HTTP MCP server for tests.
package mcptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type Server struct {
	Tools []map[string]any
	// Results maps a tool name to its CallToolResult.
	Results map[string]any
	// SSE answers with text/event-stream instead of JSON.
	SSE bool
	// Token, if set, is the required bearer token.
	Token string
	// ExpireOnce makes the first post-initialize call answer 404 (session gone).
	ExpireOnce bool

	mu      sync.Mutex
	session int
	Inits   int
	Calls   []map[string]any
	expired bool
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		ID     *int64         `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Method == "initialize" {
		s.session++
		s.Inits++
		w.Header().Set("Mcp-Session-Id", fmt.Sprintf("s%d", s.session))
		s.reply(w, req.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}})
		return
	}
	if r.Header.Get("Mcp-Session-Id") != fmt.Sprintf("s%d", s.session) {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if req.ID == nil { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if s.ExpireOnce && !s.expired {
		s.expired = true
		http.Error(w, "session expired", http.StatusNotFound)
		return
	}
	switch req.Method {
	case "tools/list":
		// Two pages, to exercise pagination.
		half := len(s.Tools) / 2
		if req.Params["cursor"] == "p2" {
			s.reply(w, req.ID, map[string]any{"tools": s.Tools[half:]})
		} else {
			s.reply(w, req.ID, map[string]any{"tools": s.Tools[:half], "nextCursor": "p2"})
		}
	case "tools/call":
		s.Calls = append(s.Calls, req.Params)
		name, _ := req.Params["name"].(string)
		res, ok := s.Results[name]
		if !ok {
			s.replyErr(w, req.ID, -32602, "unknown tool "+name)
			return
		}
		s.reply(w, req.ID, res)
	default:
		s.replyErr(w, req.ID, -32601, "method not found")
	}
}

func (s *Server) reply(w http.ResponseWriter, id *int64, result any) {
	s.write(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *Server) replyErr(w http.ResponseWriter, id *int64, code int, msg string) {
	s.write(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *Server) write(w http.ResponseWriter, msg any) {
	data, _ := json.Marshal(msg)
	if s.SSE {
		w.Header().Set("Content-Type", "text/event-stream")
		// A server notification first, which the client must skip.
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
