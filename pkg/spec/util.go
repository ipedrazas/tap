package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
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
