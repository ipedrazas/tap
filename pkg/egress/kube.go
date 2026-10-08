package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// APIStore reads keys and policies straight from the Kubernetes API
// (Secret egress-keys, ConfigMap egress-policy). Mounted volumes lag by up to
// a minute after a patch; this sees a newly deployed agent on its first call.
// The proxy's Role may only get those two named objects.
type APIStore struct {
	Namespace, SecretName, ConfigMapName string
	// MaxAge bounds how stale a cached copy may be; a miss refetches sooner.
	MaxAge time.Duration

	client *http.Client
	host   string

	mu      sync.Mutex
	keys    map[string][]byte
	policy  map[string]string
	fetched time.Time
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
	}, nil
}

func (s *APIStore) get(ctx context.Context, path string, out any) error {
	tok, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// refresh refetches when the cache is older than MaxAge, or when force is
// set and the last fetch is more than a second old (bounds API load on misses).
func (s *APIStore) refresh(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	age := time.Since(s.fetched)
	if age < s.MaxAge && (!force || age < time.Second) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var sec struct {
		Data map[string]string `json:"data"`
	}
	if err := s.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", s.Namespace, s.SecretName), &sec); err != nil {
		return err
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := s.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", s.Namespace, s.ConfigMapName), &cm); err != nil {
		return err
	}
	keys := map[string][]byte{}
	for agent, v := range sec.Data {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err == nil {
			keys[agent] = []byte(strings.TrimSpace(string(raw)))
		}
	}
	s.keys, s.policy, s.fetched = keys, cm.Data, time.Now()
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
	var k []byte
	err := s.lookup(agent, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		k = s.keys[agent]
		return k != nil
	})
	if err != nil {
		return nil, fmt.Errorf("no key: %w", err)
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
		raw = s.policy[agent+".json"]
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
