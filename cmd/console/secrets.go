package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/ipedrazas/tap/pkg/spec"
)

// maxSecret bounds one secret value set from the console.
const maxSecret = 32 << 10

var externalSecrets = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}

// SecretStatus is one secret an agent declares, as the console shows it.
// Values are never read back: OpenBao only tells the console whether the
// secret exists and when it last changed.
type SecretStatus struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Present bool      `json:"present"`
	Updated time.Time `json:"updated,omitzero"`
	Error   string    `json:"error,omitempty"`
}

// toolSecrets reads the agent's tool-secrets ExternalSecret (rendered by
// tapctl from the declared secrets): each secret's name and OpenBao path,
// and whether the last sync worked. No ExternalSecret means none declared.
func toolSecrets(ctx context.Context, dyn dynamic.Interface, namespace string) (secrets []SecretStatus, synced string, err error) {
	es, err := dyn.Resource(externalSecrets).Namespace(namespace).Get(ctx, "tool-secrets", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	for _, d := range data {
		m, _ := d.(map[string]any)
		name, _, _ := unstructured.NestedString(m, "secretKey")
		key, _, _ := unstructured.NestedString(m, "remoteRef", "key")
		if name != "" && key != "" {
			secrets = append(secrets, SecretStatus{Name: name, Path: key})
		}
	}
	conds, _, _ := unstructured.NestedSlice(es.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" {
			if m["status"] == "True" {
				synced = "synced"
			} else {
				synced, _ = m["message"].(string)
				if synced == "" {
					synced = "not synced"
				}
			}
		}
	}
	return secrets, synced, nil
}

// forceSync makes External Secrets Operator refetch now instead of at the next
// hourly refresh; the runner reads secret files on every call, so the new
// value is used without a restart once the kubelet updates the mount.
func forceSync(ctx context.Context, dyn dynamic.Interface, namespace string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		"force-sync": strconv.FormatInt(time.Now().Unix(), 10),
	}}})
	_, err := dyn.Resource(externalSecrets).Namespace(namespace).Patch(ctx, "tool-secrets", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// openBao writes agent secrets as the console's own identity: Kubernetes auth
// with a projected token for the tap-console service account, role
// tap-console, whose policy may create and update secret/data/tap/* and read
// metadata, never values.
type openBao struct {
	cfg     spec.Secrets
	role    string
	jwtFile string
	http    *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

var errNotFound = errors.New("not found")

func (b *openBao) login(ctx context.Context, force bool) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !force && b.token != "" && time.Now().Before(b.expires) {
		return b.token, nil
	}
	jwt, err := os.ReadFile(b.jwtFile)
	if err != nil {
		return "", err
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	body := map[string]string{"role": b.role, "jwt": strings.TrimSpace(string(jwt))}
	if err := b.do(ctx, "", http.MethodPost, "auth/"+b.cfg.AuthMount+"/login", body, &out); err != nil {
		return "", fmt.Errorf("openbao login: %w", err)
	}
	b.token = out.Auth.ClientToken
	b.expires = time.Now().Add(time.Duration(out.Auth.LeaseDuration)*time.Second - 30*time.Second)
	return b.token, nil
}

func (b *openBao) do(ctx context.Context, token, method, path string, in, out any) error {
	var body *bytes.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(b.cfg.Server, "/")+"/v1/"+path, body)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode >= 300:
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.Join(e.Errors, "; "))
	case out != nil:
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// call logs in (again, once, if the token was revoked or expired early).
func (b *openBao) call(ctx context.Context, method, path string, in, out any) error {
	tok, err := b.login(ctx, false)
	if err != nil {
		return err
	}
	err = b.do(ctx, tok, method, path, in, out)
	if err != nil && strings.Contains(err.Error(), ": 403 ") {
		if tok, err = b.login(ctx, true); err != nil {
			return err
		}
		err = b.do(ctx, tok, method, path, in, out)
	}
	return err
}

// status fills in Present and Updated from the secret's KV metadata.
func (b *openBao) status(ctx context.Context, s *SecretStatus) {
	var out struct {
		Data struct {
			UpdatedTime time.Time `json:"updated_time"`
		} `json:"data"`
	}
	switch err := b.call(ctx, http.MethodGet, b.cfg.KVMount+"/metadata/"+s.Path, nil, &out); {
	case errors.Is(err, errNotFound):
	case err != nil:
		s.Error = err.Error()
	default:
		s.Present, s.Updated = true, out.Data.UpdatedTime
	}
}

func (b *openBao) put(ctx context.Context, path, value string) error {
	return b.call(ctx, http.MethodPost, b.cfg.KVMount+"/data/"+path, map[string]any{"data": map[string]string{"value": value}}, nil)
}

// ownPath checks that an ExternalSecret path is inside the agent's own
// OpenBao prefix (<prefix>/<namespace>/), so a tampered ExternalSecret cannot
// steer a write to another agent's secret.
func ownPath(p *spec.Platform, namespace, path string) bool {
	prefix := p.Secrets.Prefix + "/" + namespace + "/"
	rest, ok := strings.CutPrefix(path, prefix)
	return ok && rest != "" && !strings.Contains(rest, "..") && !strings.HasPrefix(rest, "/")
}
