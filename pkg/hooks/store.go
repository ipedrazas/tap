package hooks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// ConfigMap holds hook settings (shared with the console's titles).
	ConfigMap = "agent-settings"
	// Secret holds hook credentials.
	Secret = "agent-hooks"
)

var ErrNotFound = errors.New("no such hook")

// Store reads and writes hooks in a namespace. The console uses List, Create,
// Update, SetCredential and Delete; it needs get and update on the ConfigMap
// and only patch on the Secret. tap-hooks uses Load, with get on both.
type Store struct {
	C         kubernetes.Interface
	Namespace string
}

// List returns an agent's hooks, sorted by name.
func (s *Store) List(ctx context.Context, agent string) ([]Hook, error) {
	cm, err := s.C.CoreV1().ConfigMaps(s.Namespace).Get(ctx, ConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Hook
	for k, v := range cm.Data {
		if !strings.HasPrefix(k, agent+".hook.") {
			continue
		}
		var h Hook
		if json.Unmarshal([]byte(v), &h) == nil && h.Agent == agent {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b Hook) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Create stores a new hook's settings; it fails if the name is taken.
func (s *Store) Create(ctx context.Context, h Hook) error {
	return s.write(ctx, h, func(exists bool) error {
		if exists {
			return fmt.Errorf("hook %s already exists", h.Name)
		}
		return nil
	})
}

// Update replaces an existing hook's settings.
func (s *Store) Update(ctx context.Context, h Hook) error {
	return s.write(ctx, h, func(exists bool) error {
		if !exists {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) write(ctx context.Context, h Hook, check func(exists bool) error) error {
	if err := h.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return s.updateConfig(ctx, func(data map[string]string) error {
		_, exists := data[h.ConfigKey()]
		if err := check(exists); err != nil {
			return err
		}
		data[h.ConfigKey()] = string(raw)
		return nil
	})
}

// Delete removes a hook's settings and credential.
func (s *Store) Delete(ctx context.Context, agent, name string) error {
	h := Hook{Agent: agent, Name: name}
	err := s.updateConfig(ctx, func(data map[string]string) error {
		if _, ok := data[h.ConfigKey()]; !ok {
			return ErrNotFound
		}
		delete(data, h.ConfigKey())
		return nil
	})
	if err != nil {
		return err
	}
	return s.patchSecret(ctx, h.Key(), nil)
}

// SetCredential stores a hook's credential (see NewCredential) without
// reading the Secret.
func (s *Store) SetCredential(ctx context.Context, agent, name, stored string) error {
	h := Hook{Agent: agent, Name: name}
	return s.patchSecret(ctx, h.Key(), &stored)
}

func (s *Store) updateConfig(ctx context.Context, change func(map[string]string) error) error {
	cms := s.C.CoreV1().ConfigMaps(s.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := cms.Get(ctx, ConfigMap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ConfigMap %s/%s is missing; run task platform:up", s.Namespace, ConfigMap)
		}
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if err := change(cm.Data); err != nil {
			return err
		}
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

// patchSecret sets (or, with nil, removes) one key with a merge patch.
func (s *Store) patchSecret(ctx context.Context, key string, value *string) error {
	// []byte marshals as base64, as Secret data wants; nil removes the key.
	var v []byte
	if value != nil {
		v = []byte(*value)
	}
	patch, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
	_, err := s.C.CoreV1().Secrets(s.Namespace).Patch(ctx, Secret, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("the Secret %s/%s is missing; run task platform:up", s.Namespace, Secret)
	}
	return err
}

// Entry is a hook with its stored credential, as tap-hooks needs it.
type Entry struct {
	Hook
	Credential string
}

// Load reads every hook and credential.
func (s *Store) Load(ctx context.Context) (map[string]*Entry, error) {
	cm, err := s.C.CoreV1().ConfigMaps(s.Namespace).Get(ctx, ConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	sec, err := s.C.CoreV1().Secrets(s.Namespace).Get(ctx, Secret, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]*Entry{}
	for k, v := range cm.Data {
		if !strings.Contains(k, ".hook.") {
			continue
		}
		var h Hook
		if json.Unmarshal([]byte(v), &h) != nil || h.ConfigKey() != k {
			continue
		}
		e := &Entry{Hook: h, Credential: string(sec.Data[h.Key()])}
		out[h.Key()] = e
	}
	return out, nil
}

// Cache serves lookups from a snapshot at most TTL old, so a flood of
// deliveries does not become a flood of API calls. Changes made in the
// console take effect within TTL.
type Cache struct {
	Store *Store
	TTL   time.Duration
	// MaxStale bounds how long a snapshot keeps serving while reloads fail
	// (default 10m). After that lookups fail closed: a hook removed in the
	// console must not keep working for long because the API is unreachable.
	MaxStale time.Duration

	load    singleflight.Group
	mu      sync.Mutex
	loaded  time.Time
	entries map[string]*Entry
	failed  time.Time
}

// Lookup returns the hook, or ErrNotFound.
func (c *Cache) Lookup(ctx context.Context, agent, name string) (*Entry, error) {
	entries, err := c.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	e, ok := entries[agent+"."+name]
	if !ok {
		return nil, ErrNotFound
	}
	return e, nil
}

func (c *Cache) snapshot(ctx context.Context) (map[string]*Entry, error) {
	c.mu.Lock()
	entries, loaded, failed := c.entries, c.loaded, c.failed
	c.mu.Unlock()
	if entries != nil && time.Since(loaded) <= c.TTL {
		return entries, nil
	}
	stale := func(err error) (map[string]*Entry, error) {
		if entries != nil && time.Since(loaded) <= cmp.Or(c.MaxStale, 10*time.Minute) {
			return entries, nil
		}
		return nil, err
	}
	// While the API is failing, answer at once instead of queueing every
	// delivery behind another slow call.
	if time.Since(failed) < 2*time.Second {
		return stale(errors.New("hook settings are unavailable"))
	}
	ch := c.load.DoChan("", func() (any, error) {
		// Not the request's context: a sender that hangs up must not fail
		// the load for every other delivery.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		entries, err := c.Store.Load(lctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.failed = time.Now()
			return nil, err
		}
		c.entries, c.loaded = entries, time.Now()
		return entries, nil
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return stale(r.Err)
		}
		return r.Val.(map[string]*Entry), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
