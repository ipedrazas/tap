package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// MCPEgress derives the host:port an MCP server URL needs.
func MCPEgress(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("%q: MCP servers must use https", raw)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname() + ":" + port, nil
}

// SpecHash is the sha256 of the canonical agent.yaml, recorded on the bundle
// image for provenance.
func (b *Bundle) SpecHash() string {
	sum := sha256.Sum256(canonical(b.Raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeAny(raw []byte) (any, error) {
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}

// encoding/json writes map keys in sorted order, which is all canonical needs.
func marshalSorted(v any) ([]byte, error) { return json.Marshal(v) }

// Key returns the secret's key within the agent's own OpenBao path:
// vault://<agent>/<key> gives <key>.
func (s Secret) Key(agent string) (string, error) {
	rest, ok := strings.CutPrefix(s.From, "vault://"+agent+"/")
	if !ok || rest == "" {
		return "", fmt.Errorf("%q must be vault://%s/<key>: an agent can only read its own secrets", s.From, agent)
	}
	return rest, nil
}
