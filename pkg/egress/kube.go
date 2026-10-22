package egress

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// PolicyLabel marks an agent's egress policy ConfigMap (rendered by tapctl
// render as egress-policy-<agent>); its value is the agent's name and the
// allowlist is in data["policy.json"].
const PolicyLabel = "tap.hiddenfield.dev/egress-policy"

// PolicyPrefix names an agent's policy ConfigMap: egress-policy-<agent>.
const PolicyPrefix = "egress-policy-"

// errNotFound is a 404 from the Kubernetes API or OpenBao.
var errNotFound = errors.New("not found")

// APIStore reads policies from the Kubernetes API and keys from OpenBao.
// Mounted volumes lag by up to a minute after a change; this sees a newly
// deployed agent on its first call.
//
//   - Policies: every ConfigMap labelled PolicyLabel in Namespace.
//   - Keys: Keys (OpenBao), when set.
//
// The legacy objects, Secret SecretName (one key per agent) and ConfigMap
// ConfigMapName (one <agent>.json per agent), are read as a fallback for
// agents not yet redeployed, and for the factory. Empty names skip them.
type APIStore struct {
	Namespace, SecretName, ConfigMapName string
	Keys                                 *OpenBao
	// MaxAge bounds how stale a cached copy may be; a miss refetches sooner.
	MaxAge time.Duration
	// MaxStale bounds how long the last good copy keeps serving while the
	// API can't be read (default 10m): an API server outage doesn't stop
	// egress, but a policy removed during one doesn't outlive it by much.
	MaxStale time.Duration

	client *http.Client
	host   string
	token  func() (string, error)

	fetches singleflight.Group
	mu      sync.Mutex
	keys    map[string][]byte // legacy
	policy  map[string]string
	fetched time.Time
	failed  time.Time
	lastErr error
}

func NewAPIStore(namespace, secret, configMap string) (*APIStore, error) {
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster")
	}
	return &APIStore{
		Namespace: namespace, SecretName: secret, ConfigMapName: configMap,
		MaxAge: 30 * time.Second,
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
		host:   "https://" + strings.Trim(host, "[]") + ":" + port,
		token:  func() (string, error) { return readToken(saDir + "/token") },
	}, nil
}

func readToken(file string) (string, error) {
	b, err := os.ReadFile(file)
	return strings.TrimSpace(string(b)), err
}

func (s *APIStore) get(ctx context.Context, path string, out any) error {
	tok, err := s.token()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		return errNotFound
	default:
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
}

type object struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

// refresh refetches when the cache is older than MaxAge, or when force is
// set and the last fetch is more than a second old (bounds API load on
// misses). A failed fetch is retried at most once a second, and meanwhile
// the last good copy is served, up to MaxStale old. Fetches run outside the
// lock, one at a time.
func (s *APIStore) refresh(force bool) error {
	s.mu.Lock()
	age := time.Since(s.fetched)
	fresh := s.policy != nil && age < s.MaxAge && (!force || age < time.Second)
	usable := s.policy != nil && age < cmp.Or(s.MaxStale, 10*time.Minute)
	backoff, lastErr := time.Since(s.failed) < time.Second, s.lastErr
	s.mu.Unlock()
	switch {
	case fresh:
		return nil
	case backoff && usable:
		return nil
	case backoff:
		return lastErr
	}
	_, err, _ := s.fetches.Do("", func() (any, error) { return nil, s.fetch() })
	if err != nil {
		s.mu.Lock()
		s.failed, s.lastErr = time.Now(), err
		s.mu.Unlock()
		if usable {
			return nil
		}
	}
	return err
}

func (s *APIStore) fetch() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	policy := map[string]string{}
	if s.ConfigMapName != "" {
		var cm object
		err := s.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", s.Namespace, s.ConfigMapName), &cm)
		if err != nil && !errors.Is(err, errNotFound) {
			return err
		}
		for k, v := range cm.Data {
			if agent, ok := strings.CutSuffix(k, ".json"); ok {
				policy[agent] = v
			}
		}
	}
	var list struct {
		Items []object `json:"items"`
	}
	if err := s.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/configmaps?labelSelector=%s", s.Namespace, url.QueryEscape(PolicyLabel)), &list); err != nil {
		return err
	}
	// Labelled ConfigMaps win over the legacy one. Only egress-policy-<agent>
	// speaks for <agent>: a label on any other ConfigMap can't claim it, and
	// names are unique, so neither can a second one.
	for _, cm := range list.Items {
		if agent := cm.Metadata.Labels[PolicyLabel]; agent != "" && cm.Metadata.Name == PolicyPrefix+agent && cm.Data["policy.json"] != "" {
			policy[agent] = cm.Data["policy.json"]
		}
	}
	keys := map[string][]byte{}
	if s.SecretName != "" {
		var sec object
		err := s.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", s.Namespace, s.SecretName), &sec)
		if err != nil && !errors.Is(err, errNotFound) {
			return err
		}
		for agent, v := range sec.Data {
			raw, err := base64.StdEncoding.DecodeString(v)
			if err == nil {
				keys[agent] = []byte(strings.TrimSpace(string(raw)))
			}
		}
	}
	s.mu.Lock()
	s.keys, s.policy, s.fetched = keys, policy, time.Now()
	s.mu.Unlock()
	return nil
}

func (s *APIStore) lookup(agent string, find func() bool) error {
	if !agentName.MatchString(agent) {
		return fmt.Errorf("invalid agent name")
	}
	if err := s.refresh(false); err != nil {
		return err
	}
	if find() {
		return nil
	}
	if err := s.refresh(true); err != nil {
		return err
	}
	if !find() {
		return fmt.Errorf("not configured")
	}
	return nil
}

func (s *APIStore) Key(agent string) ([]byte, error) {
	if !agentName.MatchString(agent) {
		return nil, fmt.Errorf("no key: invalid agent name")
	}
	var k []byte
	var baoErr error
	if s.Keys != nil {
		k, baoErr = s.Keys.Key(agent)
	}
	if k == nil {
		// Not in OpenBao (or OpenBao is unreachable): the legacy Secret.
		err := s.lookup(agent, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			k = s.keys[agent]
			return k != nil
		})
		if err != nil {
			return nil, fmt.Errorf("no key: %w", errors.Join(baoErr, err))
		}
	}
	if len(k) < 32 {
		return nil, fmt.Errorf("key too short")
	}
	return k, nil
}

func (s *APIStore) Policy(agent string) (Policy, error) {
	var raw string
	err := s.lookup(agent, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		raw = s.policy[agent]
		return raw != ""
	})
	if err != nil {
		return nil, fmt.Errorf("no policy: %w", err)
	}
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	return p, nil
}
