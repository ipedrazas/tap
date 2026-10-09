package harness

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// IdentityHeader carries the OIDC ID token forwarded by the gateway
// (SecurityPolicy forwardIDToken). The gateway has verified it, and the
// NetworkPolicy admits traffic only from the gateway, so the harness reads
// the claims without re-verifying.
const IdentityHeader = "x-tap-identity"

type Server struct {
	Agent     *Agent
	Workspace string
	Logger    *slog.Logger
	// Exporter, when set, uploads each session's trace after every turn.
	Exporter *Exporter

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	mu      sync.Mutex
	History []Message `json:"history"`
	Trace   *Trace    `json:"trace,omitempty"`
}

var sessionID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		_, _ = w.Write(uiHTML)
	})
	mux.HandleFunc("GET /v1/info", s.info)
	mux.HandleFunc("POST /v1/chat", s.chat)
	return mux
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	a := s.Agent.Bundle.Agent
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":        a.Metadata.Name,
		"version":     a.Metadata.Version,
		"description": a.Metadata.Description,
		"model":       a.Harness.Model.Name,
		"tools":       s.Agent.Bundle.ToolNames(),
		"user":        user(r),
	})
}

type chatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// chat runs one turn and streams events as server-sent events:
// session, tool_call, tool_result, message, error, done.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		http.Error(w, "want {\"message\": \"...\", \"session_id\": optional}", http.StatusBadRequest)
		return
	}
	if req.SessionID != "" && !sessionID.MatchString(req.SessionID) {
		http.Error(w, "bad session_id", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	who := user(r)
	id := req.SessionID
	if id == "" {
		id = newID()
	}
	sess := s.session(who, id)
	if !sess.mu.TryLock() {
		http.Error(w, "a turn is already running in this session", http.StatusConflict)
		return
	}
	defer sess.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}
	send("session", map[string]string{"session_id": id})

	s.Logger.Info("turn", "user", who, "session", id)
	if sess.Trace == nil {
		sess.Trace = newTrace(id, s.Agent.Bundle, userKey(who))
	}
	turn := sess.Trace.beginTurn(req.Message, time.Now())
	history, err := s.Agent.Turn(r.Context(), sess.History, req.Message, turn, func(e Event) { send(e.Type, e) })
	sess.Trace.endTurn(turn, time.Now())
	sess.History = history
	if err != nil {
		s.Logger.Error("turn failed", "session", id, "err", err)
		send("error", Event{Type: "error", Content: "the agent could not complete this turn"})
	}
	if err := s.persist(who, id, sess); err != nil {
		s.Logger.Error("persist session", "err", err)
	}
	if s.Exporter != nil {
		if data, err := json.Marshal(sess.Trace); err == nil {
			s.Exporter.Enqueue(TraceKey(sess.Trace), data)
		}
	}
	send("done", map[string]string{})
}

func (s *Server) session(who, id string) *session {
	key := userKey(who) + "/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]*session{}
	}
	if sess, ok := s.sessions[key]; ok {
		return sess
	}
	sess := &session{}
	if data, err := os.ReadFile(s.sessionPath(who, id)); err == nil {
		_ = json.Unmarshal(data, sess)
	}
	s.sessions[key] = sess
	return sess
}

// TraceKey is where a session's trace is stored: one prefix per agent, so a
// reader can list one agent's sessions (or all of them) without recursion.
func TraceKey(t *Trace) string {
	return t.AgentName + "/" + t.ID + ".trace.json"
}

func (s *Server) sessionPath(who, id string) string {
	return filepath.Join(s.Workspace, "sessions", userKey(who), id+".json")
}

func (s *Server) persist(who, id string, sess *session) error {
	p := s.sessionPath(who, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// user returns the email (or subject) from the forwarded ID token.
func user(r *http.Request) string {
	tok := r.Header.Get(IdentityHeader)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "anonymous"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "anonymous"
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "anonymous"
	}
	if claims.Email != "" {
		return claims.Email
	}
	if claims.Sub != "" {
		return claims.Sub
	}
	return "anonymous"
}

func userKey(who string) string {
	sum := sha256.Sum256([]byte(who))
	return hex.EncodeToString(sum[:8])
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
