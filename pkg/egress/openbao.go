package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenBao reads agent egress keys from <KVMount>/<Prefix>/<NamespacePrefix><agent>,
// property "key", where each agent namespace's PushSecret writes them. It logs
// in with Kubernetes auth (role Role at AuthMount) using the projected service
// account token in TokenFile.
//
// Keys are cached for MaxAge. A miss is retried at most once a second per
// agent. When OpenBao can't be reached, a cached key keeps working, so an
// OpenBao outage doesn't stop agents that are already running.
type OpenBao struct {
	Addr, AuthMount, Role, KVMount, Prefix, NamespacePrefix string
	TokenFile                                               string
	MaxAge                                                  time.Duration
	Client                                                  *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	keys    map[string]cachedKey
}

type cachedKey struct {
	key     []byte // nil: not in OpenBao
	fetched time.Time
}

// Key returns the agent's key, or nil when OpenBao has none.
func (o *OpenBao) Key(agent string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.keys[agent]
	age := time.Since(c.fetched)
	if ok && (age < o.MaxAge && c.key != nil || age < time.Second) {
		return c.key, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, err := o.read(ctx, agent)
	if err != nil {
		if c.key != nil {
			// Keep serving it, and don't retry until MaxAge has passed again.
			o.keys[agent] = cachedKey{key: c.key, fetched: time.Now()}
			return c.key, nil
		}
		return nil, err
	}
	if o.keys == nil {
		o.keys = map[string]cachedKey{}
	}
	o.keys[agent] = cachedKey{key: key, fetched: time.Now()}
	return key, nil
}

func (o *OpenBao) read(ctx context.Context, agent string) ([]byte, error) {
	path := fmt.Sprintf("/v1/%s/data/%s/%s%s", o.KVMount, o.Prefix, o.NamespacePrefix, agent)
	for attempt := 0; ; attempt++ {
		tok, err := o.login(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		var out struct {
			Data struct {
				Data map[string]string `json:"data"`
			} `json:"data"`
		}
		status, err := o.do(ctx, http.MethodGet, path, tok, nil, &out)
		switch {
		case err != nil:
			return nil, err
		case status == http.StatusNotFound:
			return nil, nil
		case status == http.StatusForbidden && attempt == 0:
			continue // the token may have been revoked: log in again
		case status != http.StatusOK:
			return nil, fmt.Errorf("openbao: GET %s: %d", path, status)
		}
		k := strings.TrimSpace(out.Data.Data["key"])
		if k == "" {
			return nil, nil
		}
		return []byte(k), nil
	}
}

// login returns a token, logging in again when forced or within a minute of
// expiry. Callers hold o.mu.
func (o *OpenBao) login(ctx context.Context, force bool) (string, error) {
	if !force && o.token != "" && time.Until(o.expires) > time.Minute {
		return o.token, nil
	}
	jwt, err := readToken(o.TokenFile)
	if err != nil {
		return "", err
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	path := fmt.Sprintf("/v1/auth/%s/login", o.AuthMount)
	status, err := o.do(ctx, http.MethodPost, path, "", map[string]string{"role": o.Role, "jwt": jwt}, &out)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || out.Auth.ClientToken == "" {
		return "", fmt.Errorf("openbao: login as %s: %d", o.Role, status)
	}
	o.token, o.expires = out.Auth.ClientToken, time.Now().Add(time.Duration(out.Auth.LeaseDuration)*time.Second)
	return o.token, nil
}

func (o *OpenBao) do(ctx context.Context, method, path, token string, body, out any) (int, error) {
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.Addr, "/")+path, r)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return 0, errors.Join(fmt.Errorf("openbao: %s %s", method, path), err)
		}
	}
	return resp.StatusCode, nil
}
