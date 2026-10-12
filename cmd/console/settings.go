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

// platformFile is platform.yaml as mounted from tap-system/tap-platform. It is
// reread when it changes, so `task console:config` takes effect without a
// restart (the kubelet refreshes the mount within a minute).
type platformFile struct {
	path string

	mu      sync.Mutex
	mtime   time.Time
	current *spec.Platform
}

func (f *platformFile) get() (*spec.Platform, error) {
	if f == nil || f.path == "" {
		return nil, fmt.Errorf("no platform.yaml configured")
	}
	st, err := os.Stat(f.path)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current != nil && st.ModTime().Equal(f.mtime) {
		return f.current, nil
	}
	p, err := spec.LoadPlatform(f.path)
	if err != nil {
		return nil, err
	}
	f.current, f.mtime = p, st.ModTime()
	return p, nil
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
