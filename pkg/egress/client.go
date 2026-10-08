package egress

import (
	"fmt"
	"time"
)

// Minter gives a tool process the environment that routes it through the
// proxy with a credential valid only for its own scope.
type Minter struct {
	ProxyAddr string // host:port
	Agent     string
	Key       []byte
}

func (m Minter) Env(scope string, ttl time.Duration) ([]string, error) {
	tok, err := Mint(m.Key, Claims{Agent: m.Agent, Scope: scope, Expires: time.Now().Add(ttl).Unix()})
	if err != nil {
		return nil, err
	}
	return ProxyEnv(fmt.Sprintf("http://%s:%s@%s", m.Agent, tok, m.ProxyAddr)), nil
}

// ProxyEnv covers the conventions of curl, Python, Go and Node (Node's
// built-in fetch honours these only with NODE_USE_ENV_PROXY=1).
func ProxyEnv(url string) []string {
	return []string{
		"HTTPS_PROXY=" + url, "https_proxy=" + url,
		"HTTP_PROXY=" + url, "http_proxy=" + url,
		"NO_PROXY=", "no_proxy=",
		"NODE_USE_ENV_PROXY=1",
	}
}
