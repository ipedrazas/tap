// Package egress enforces per-call network allowlists. The runner mints a
// short-lived token naming the agent and the tool making the call; the proxy
// verifies it with the agent's key and allows only the hosts that tool
// declares in agent.yaml. The allowlist comes from the proxy's own policy, not
// the token, so a leaked key cannot widen what an agent may reach.
package egress

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Claims struct {
	Agent string `json:"a"`
	// Scope is a tool name, or "mcp:<server>" for MCP connections.
	Scope   string `json:"s"`
	Expires int64  `json:"e"`
	Nonce   string `json:"n"`
}

var b64 = base64.RawURLEncoding

// Mint returns "<payload>.<mac>".
func Mint(key []byte, c Claims) (string, error) {
	if c.Nonce == "" {
		n := make([]byte, 8)
		if _, err := rand.Read(n); err != nil {
			return "", err
		}
		c.Nonce = hex.EncodeToString(n)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := b64.EncodeToString(payload)
	return p + "." + b64.EncodeToString(sign(key, p)), nil
}

func sign(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("bad signature")
	ErrExpired   = errors.New("token expired")
)

// Verify checks the token against keyFor(agent) and returns its claims.
func Verify(token string, keyFor func(agent string) ([]byte, error), now time.Time) (Claims, error) {
	var c Claims
	p, mac, ok := strings.Cut(token, ".")
	if !ok {
		return c, ErrMalformed
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return c, ErrMalformed
	}
	if err := json.Unmarshal(payload, &c); err != nil || c.Agent == "" || c.Scope == "" {
		return c, ErrMalformed
	}
	key, err := keyFor(c.Agent)
	if err != nil {
		return c, fmt.Errorf("agent %s: %w", c.Agent, err)
	}
	got, err := b64.DecodeString(mac)
	if err != nil || !hmac.Equal(got, sign(key, p)) {
		return c, ErrSignature
	}
	if now.Unix() > c.Expires {
		return c, ErrExpired
	}
	return c, nil
}
