package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ipedrazas/tap/pkg/inventory"
	"github.com/ipedrazas/tap/pkg/spec"
)

const maxTitle = 80

// platformFile is platform.yaml as mounted from tap-system/tap-platform-<hash>.
// A platform.yaml change makes a new ConfigMap and rolls the console, but the
// file is still reread when it changes, for a ConfigMap edited in place (the
// kubelet refreshes the mount within a minute). A reread that
// fails keeps the last platform.yaml that loaded, so a file this build cannot
// parse (say, a field added after it was built) does not lock every agent.
type platformFile struct {
	path string

	mu      sync.Mutex
	mtime   time.Time
	current *spec.Platform
	err     error // the last reread's failure; nil once one succeeds
}

// get returns the current platform.yaml. On error it may still return the
// last version that loaded, which callers can keep using.
func (f *platformFile) get() (*spec.Platform, error) {
	if f == nil || f.path == "" {
		return nil, fmt.Errorf("no platform.yaml configured")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(f.path)
	if err != nil {
		f.err = err
		return f.current, err
	}
	if f.current != nil && st.ModTime().Equal(f.mtime) {
		return f.current, nil
	}
	p, err := spec.LoadPlatform(f.path)
	if err != nil {
		f.err = err
		return f.current, err
	}
	f.current, f.mtime, f.err = p, st.ModTime(), nil
	return p, nil
}

// loadErr is why the last reread failed, or nil.
func (f *platformFile) loadErr() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// canEdit reports whether email belongs to the team that owns the agent
// (platform.yaml teams[owner]). An empty email or unknown owner never can.
func canEdit(p *spec.Platform, email, owner string) bool {
	if p == nil || email == "" {
		return false
	}
	return slices.ContainsFunc(p.Teams[owner], func(m string) bool { return strings.EqualFold(m, email) })
}

// titles is the console's own per-agent settings: ConfigMap agent-settings in
// the console's namespace, one "<agent>.title" key per override. The console
// may get and update only that object.
type titles struct {
	c         kubernetes.Interface
	namespace string
}

const settingsConfigMap = "agent-settings"

func (t *titles) all(ctx context.Context) (map[string]string, error) {
	cm, err := t.c.CoreV1().ConfigMaps(t.namespace).Get(ctx, settingsConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range cm.Data {
		if name, ok := strings.CutSuffix(k, ".title"); ok {
			out[name] = v
		}
	}
	return out, nil
}

// set stores an override; "" removes it so the spec title (or name) shows.
func (t *titles) set(ctx context.Context, agent, title string) error {
	cms := t.c.CoreV1().ConfigMaps(t.namespace)
	cm, err := cms.Get(ctx, settingsConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("ConfigMap %s/%s is missing; run task platform:up", t.namespace, settingsConfigMap)
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	if title == "" {
		delete(cm.Data, agent+".title")
	} else {
		cm.Data[agent+".title"] = title
	}
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func checkTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > maxTitle || strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("title must be one line of at most %d characters", maxTitle)
	}
	return s, nil
}

// apply fills in each agent's TitleOverride. A failure to read the
// settings only loses the overrides.
func (t *titles) apply(ctx context.Context, agents []inventory.Agent) error {
	m, err := t.all(ctx)
	if err != nil {
		return err
	}
	for i := range agents {
		agents[i].TitleOverride = m[agents[i].Name]
	}
	return nil
}
