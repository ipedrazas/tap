// Package hooks lets external systems (GitHub, CI, anything that can POST
// JSON) start a run of an agent. A hook belongs to one agent and has its own
// credential, an optional event filter and a prompt template that turns the
// delivery into the message the agent receives.
//
// Hooks are configured in the console. Their settings live in the console's
// ConfigMap agent-settings (key "<agent>.hook.<name>") and their credentials
// in Secret agent-hooks (key "<agent>.<name>"), which the console may only
// patch, never read. tap-hooks reads both, verifies each delivery and asks
// the agent's harness to run a turn.
package hooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"text/template"
	tparse "text/template/parse"
	"time"
)

// Auth is how a delivery proves it comes from the configured sender.
const (
	// AuthGitHub: X-Hub-Signature-256 is sha256=HMAC-SHA256(secret, body),
	// as GitHub, Gitea and Forgejo send it. The secret is stored as is.
	AuthGitHub = "github"
	// AuthBearer: Authorization: Bearer <token>. Only the token's SHA-256
	// is stored.
	AuthBearer = "bearer"
)

const (
	// MaxPrompt bounds a prompt template; MaxRendered the message it makes.
	MaxPrompt   = 8 << 10
	MaxRendered = 32 << 10
	// MaxEvents bounds a hook's event filter.
	MaxEvents = 32
	// TokenPrefix marks bearer tokens so secret scanners can find leaks.
	TokenPrefix = "taphk_"
)

var (
	NameRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	AgentRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	eventRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`)
)

// Hook is one hook's settings. Its credential is stored separately.
type Hook struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Auth    string `json:"auth"`
	// Events limits which deliveries start a run: "pull_request" matches
	// every action of that event, "pull_request.opened" one action. Empty
	// accepts every delivery.
	Events []string `json:"events,omitempty"`
	// Prompt is a text/template rendered over the delivery (see Data).
	Prompt    string    `json:"prompt"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	Updated   time.Time `json:"updated,omitzero"`
}

// Key is the hook's key in Secret agent-hooks; ConfigKey in agent-settings.
func (h *Hook) Key() string       { return h.Agent + "." + h.Name }
func (h *Hook) ConfigKey() string { return h.Agent + ".hook." + h.Name }

// Validate checks everything a console user can set.
func (h *Hook) Validate() error {
	if !AgentRE.MatchString(h.Agent) {
		return fmt.Errorf("bad agent name %q", h.Agent)
	}
	if !NameRE.MatchString(h.Name) {
		return fmt.Errorf("hook name must match %s", NameRE)
	}
	if h.Auth != AuthGitHub && h.Auth != AuthBearer {
		return fmt.Errorf("auth must be %q or %q", AuthGitHub, AuthBearer)
	}
	if len(h.Events) > MaxEvents {
		return fmt.Errorf("at most %d events", MaxEvents)
	}
	for _, e := range h.Events {
		if !eventRE.MatchString(e) {
			return fmt.Errorf("event %q must look like push or pull_request.opened", e)
		}
	}
	if strings.TrimSpace(h.Prompt) == "" || len(h.Prompt) > MaxPrompt {
		return fmt.Errorf("prompt must be 1 byte to %d KiB", MaxPrompt>>10)
	}
	if _, err := parse(h.Prompt); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	return nil
}

// NewCredential returns a fresh credential for auth: the value to show the
// user once, and the value to store.
func NewCredential(auth string) (show, store string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	secret := hex.EncodeToString(b)
	if auth == AuthBearer {
		token := TokenPrefix + secret
		sum := sha256.Sum256([]byte(token))
		return token, "sha256:" + hex.EncodeToString(sum[:])
	}
	return secret, secret
}

var ErrUnauthorized = errors.New("delivery is not signed with this hook's credential")

// Verify checks a delivery against the stored credential, in constant time.
func Verify(auth, stored string, header http.Header, body []byte) error {
	if stored == "" {
		return ErrUnauthorized
	}
	switch auth {
	case AuthGitHub:
		sig, ok := strings.CutPrefix(header.Get("X-Hub-Signature-256"), "sha256=")
		got, err := hex.DecodeString(sig)
		if !ok || err != nil {
			return ErrUnauthorized
		}
		mac := hmac.New(sha256.New, []byte(stored))
		mac.Write(body)
		if !hmac.Equal(got, mac.Sum(nil)) {
			return ErrUnauthorized
		}
		return nil
	case AuthBearer:
		token, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer ")
		want, found := strings.CutPrefix(stored, "sha256:")
		sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
		if !ok || !found || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) != 1 {
			return ErrUnauthorized
		}
		return nil
	}
	return ErrUnauthorized
}

// Delivery is one verified request to a hook.
type Delivery struct {
	// ID identifies the delivery for deduplication (X-GitHub-Delivery or
	// Idempotency-Key); empty when the sender gives none.
	ID string
	// Event is X-GitHub-Event or X-Tap-Event; Action the payload's "action".
	Event, Action string
	Payload       any
}

// ParseDelivery reads the event and JSON payload of a request.
func ParseDelivery(header http.Header, body []byte) (*Delivery, error) {
	d := &Delivery{
		ID:    firstHeader(header, "X-GitHub-Delivery", "X-Gitea-Delivery", "Idempotency-Key"),
		Event: firstHeader(header, "X-GitHub-Event", "X-Gitea-Event", "X-Tap-Event"),
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&d.Payload); err != nil {
		return nil, fmt.Errorf("body must be JSON (for GitHub, set the content type to application/json): %w", err)
	}
	if m, ok := d.Payload.(map[string]any); ok {
		d.Action, _ = m["action"].(string)
	}
	if len(d.ID) > 128 || len(d.Event) > 64 {
		return nil, fmt.Errorf("delivery id or event too long")
	}
	return d, nil
}

func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(h.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// Wants reports whether the hook's event filter accepts the delivery.
func (h *Hook) Wants(d *Delivery) bool {
	if len(h.Events) == 0 {
		return true
	}
	return slices.Contains(h.Events, d.Event) || (d.Action != "" && slices.Contains(h.Events, d.Event+"."+d.Action))
}

// Data is what a prompt template sees:
//
//	{{.agent}} {{.hook}} {{.event}} {{.action}} {{.delivery}} {{.payload.repository.full_name}}
//
// A missing field renders as an empty string, but a field under a missing
// one fails the delivery; guard it with {{with .payload.pull_request}}.
// Functions: json (compact JSON of a value) and truncate N (at most N
// characters).
func Data(h *Hook, d *Delivery) map[string]any {
	return map[string]any{"agent": h.Agent, "hook": h.Name, "event": d.Event, "action": d.Action, "delivery": d.ID, "payload": d.Payload}
}

var funcs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"truncate": func(n int, v any) string {
		s := fmt.Sprint(v)
		if v == nil {
			s = ""
		}
		if r := []rune(s); len(r) > n {
			return string(r[:n]) + "…"
		}
		return s
	},
}

// disabled shadows builtins that can allocate or loop without writing
// output: printf "%999999999s" allocates before the output cap sees it.
func disabled(name string) func(...any) (string, error) {
	return func(...any) (string, error) { return "", fmt.Errorf("%s is not available in hook prompts", name) }
}

func init() {
	for _, name := range []string{"printf", "print", "println"} {
		funcs[name] = disabled(name)
	}
}

func parse(prompt string) (*template.Template, error) {
	t, err := template.New("prompt").Funcs(funcs).Option("missingkey=zero").Parse(prompt)
	if err != nil {
		return nil, err
	}
	// define, block and template allow recursion; nested range is quadratic
	// in the payload. Neither writes output the cap could stop.
	if len(t.Templates()) > 1 {
		return nil, fmt.Errorf("define and block are not available in hook prompts")
	}
	if err := checkNodes(t.Tree.Root, 0); err != nil {
		return nil, err
	}
	return t, nil
}

func checkNodes(n tparse.Node, ranges int) error {
	switch n := n.(type) {
	case *tparse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := checkNodes(c, ranges); err != nil {
				return err
			}
		}
	case *tparse.TemplateNode:
		return fmt.Errorf("template is not available in hook prompts")
	case *tparse.RangeNode:
		if ranges > 0 {
			return fmt.Errorf("range inside range is not available in hook prompts")
		}
		if err := checkNodes(n.List, ranges+1); err != nil {
			return err
		}
		return checkNodes(n.ElseList, ranges)
	case *tparse.IfNode:
		if err := checkNodes(n.List, ranges); err != nil {
			return err
		}
		return checkNodes(n.ElseList, ranges)
	case *tparse.WithNode:
		if err := checkNodes(n.List, ranges); err != nil {
			return err
		}
		return checkNodes(n.ElseList, ranges)
	}
	return nil
}

// Render makes the agent's message from the hook's prompt and a delivery.
func (h *Hook) Render(d *Delivery) (string, error) {
	t, err := parse(h.Prompt)
	if err != nil {
		return "", err
	}
	w := &capped{max: MaxRendered}
	if err := t.Execute(w, Data(h, d)); err != nil {
		return "", err
	}
	// A missing map key prints as "<no value>" even with missingkey=zero.
	out := strings.TrimSpace(strings.ReplaceAll(w.String(), "<no value>", ""))
	if out == "" {
		return "", fmt.Errorf("prompt rendered empty")
	}
	return out, nil
}

// capped stops a template that would produce more than max bytes.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.max {
		return 0, fmt.Errorf("prompt renders to more than %d KiB", c.max>>10)
	}
	return c.Buffer.Write(p)
}
