package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ipedrazas/tap/pkg/hooks"
)

func TestSettingsHooks(t *testing.T) {
	e := newSettingsEnv(t, nil)
	const base = "/api/agents/demo-agent/hooks"
	create := `{"name":"prs","auth":"github","enabled":true,"events":["pull_request.opened"," ","pull_request.opened"],"prompt":"Review {{.payload.pull_request.html_url}}"}`
	if code, _ := e.do(t, "POST", base, "bob@example.com", create); code != 403 {
		t.Errorf("non-member: %d", code)
	}
	if code, body := e.do(t, "POST", base, "alice@example.com", `{"name":"prs","auth":"github","prompt":"{{.x"}`); code != 422 {
		t.Errorf("bad template: %d %s", code, body)
	}
	code, body := e.do(t, "POST", base, "alice@example.com", create)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var out hookCreated
	json.Unmarshal([]byte(body), &out)
	if out.URL != "https://hooks.example.com/h/demo-agent/prs" || len(out.Secret) != 64 {
		t.Fatalf("created %+v", out)
	}
	if code, _ := e.do(t, "POST", base, "alice@example.com", create); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
	store := &hooks.Store{C: e.k8s, Namespace: "tap-system"}
	all, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := all["demo-agent.prs"]
	if h == nil || h.Credential != out.Secret || len(h.Events) != 1 || h.UpdatedBy != "alice@example.com" {
		t.Fatalf("stored %+v", h)
	}

	// The page lists the hook but never its credential.
	_, html := e.do(t, "GET", "/agents/demo-agent", "alice@example.com", "")
	if !strings.Contains(html, `data-hook="prs"`) || !strings.Contains(html, out.URL) || strings.Contains(html, out.Secret) {
		t.Error("page does not list the hook, or shows its secret")
	}

	if code, body := e.do(t, "PUT", base+"/prs", "alice@example.com", `{"enabled":false,"events":[],"prompt":"go"}`); code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	if code, _ := e.do(t, "PUT", base+"/nope", "alice@example.com", `{"prompt":"go"}`); code != 404 {
		t.Errorf("update missing: %d", code)
	}
	code, body = e.do(t, "POST", base+"/prs/rotate", "alice@example.com", `{}`)
	var rotated hookCreated
	json.Unmarshal([]byte(body), &rotated)
	all, _ = store.Load(context.Background())
	if code != 200 || rotated.Secret == out.Secret || all["demo-agent.prs"].Credential != rotated.Secret || all["demo-agent.prs"].Enabled {
		t.Fatalf("rotate: %d %+v %+v", code, rotated, all["demo-agent.prs"])
	}

	// Bearer tokens are stored hashed.
	code, body = e.do(t, "POST", base, "alice@example.com", `{"name":"ci","auth":"bearer","prompt":"build {{.payload.ref}}"}`)
	var bearer hookCreated
	json.Unmarshal([]byte(body), &bearer)
	sec, _ := e.k8s.CoreV1().Secrets("tap-system").Get(context.Background(), hooks.Secret, metav1.GetOptions{})
	if code != 201 || !strings.HasPrefix(bearer.Secret, hooks.TokenPrefix) || strings.Contains(string(sec.Data["demo-agent.ci"]), bearer.Secret) {
		t.Fatalf("bearer: %d %s", code, body)
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+bearer.Secret)
	if err := hooks.Verify(hooks.AuthBearer, string(sec.Data["demo-agent.ci"]), hdr, nil); err != nil {
		t.Fatal(err)
	}

	if code, _ := e.do(t, "DELETE", base+"/prs", "alice@example.com", `{}`); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := e.do(t, "DELETE", base+"/prs", "alice@example.com", `{}`); code != 404 {
		t.Errorf("delete again: %d", code)
	}
	all, _ = store.Load(context.Background())
	if _, ok := all["demo-agent.prs"]; ok || len(all) != 1 {
		t.Fatalf("after delete %+v", all)
	}
}
