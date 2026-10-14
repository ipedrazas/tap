package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// An egress proxy in the tap style admits a CONNECT only with a short-lived
// credential for one scope. The proxy URL is http://<agent>:<token>@<addr>,
// and the token is "<payload>.<mac>": payload is the base64url (unpadded)
// JSON of egressClaims, mac the base64url HMAC-SHA256 of the payload string
// under the agent's egress key. The proxy keeps the allowlist for each
// scope, so the token only says who is asking and for which scope.
type egressClaims struct {
	Agent   string `json:"a"`
	Scope   string `json:"s"`
	Expires int64  `json:"e"`
	Nonce   string `json:"n"`
}

var b64 = base64.RawURLEncoding

func mintEgressToken(key []byte, c egressClaims) (string, error) {
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
	m := hmac.New(sha256.New, key)
	m.Write([]byte(p))
	return p + "." + b64.EncodeToString(m.Sum(nil)), nil
}

// egressProxy routes requests through the proxy at addr (host:port) with a
// fresh credential for every new connection. The key file is read each time,
// so a rotated key is picked up.
func egressProxy(addr, agent, scope, keyFile string) func(*http.Request) (*url.URL, error) {
	return func(*http.Request) (*url.URL, error) {
		key, err := readTrim(keyFile)
		if err != nil {
			return nil, err
		}
		tok, err := mintEgressToken([]byte(key), egressClaims{Agent: agent, Scope: scope, Expires: time.Now().Add(5 * time.Minute).Unix()})
		if err != nil {
			return nil, err
		}
		return &url.URL{Scheme: "http", User: url.UserPassword(agent, tok), Host: addr}, nil
	}
}
