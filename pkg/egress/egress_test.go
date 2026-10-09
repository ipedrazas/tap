package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ipedrazas/tap/pkg/spec"
)

var key = []byte(strings.Repeat("k", 32))

type memStore map[string]Policy

func (m memStore) Key(agent string) ([]byte, error) {
	if _, ok := m[agent]; !ok {
		return nil, fmt.Errorf("no key")
	}
	return key, nil
}
func (m memStore) Policy(agent string) (Policy, error) { return m[agent], nil }

func TestToken(t *testing.T) {
	keyFor := func(string) ([]byte, error) { return key, nil }
	tok, err := Mint(key, Claims{Agent: "a1", Scope: "fetch", Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := Verify(tok, keyFor, time.Now()); err != nil || c.Scope != "fetch" {
		t.Fatalf("verify: %v %+v", err, c)
	}
	// Changing the scope breaks the signature.
	p, mac, _ := strings.Cut(tok, ".")
	raw, _ := b64.DecodeString(p)
	forged := strings.Replace(string(raw), `"fetch"`, `"other"`, 1)
	if _, err := Verify(b64.EncodeToString([]byte(forged))+"."+mac, keyFor, time.Now()); err != ErrSignature {
		t.Fatalf("forged token: %v", err)
	}
	if _, err := Verify(tok, keyFor, time.Now().Add(2*time.Minute)); err != ErrExpired {
		t.Fatalf("expired token: %v", err)
	}
	if _, err := Verify("garbage", keyFor, time.Now()); err != ErrMalformed {
		t.Fatalf("garbage: %v", err)
	}
}

// upstream is a TLS server on 127.0.0.1 standing in for a public API.
func upstream(t *testing.T) (*httptest.Server, string) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return srv, "example.com:" + u.Port()
}

// toLoopback resolves every name to 127.0.0.1, where the test upstream listens.
func toLoopback(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

func clientVia(t *testing.T, proxyURL string, roots *x509.CertPool) *http.Client {
	pu, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{RootCAs: roots},
	}}
}

func TestProxy(t *testing.T) {
	up, target := upstream(t)
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	_, port, _ := strings.Cut(target, ":")
	store := memStore{"a1": {"fetch": {target}}, "a2": {}, "a3": {"bash": {"*:" + port}, "web": {"*:1"}}}
	p := &Proxy{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AllowPrivate: true, Lookup: toLoopback}
	ps := httptest.NewServer(p)
	defer ps.Close()
	addr := strings.TrimPrefix(ps.URL, "http://")
	mint := func(agent, scope string) string {
		tok, _ := Mint(key, Claims{Agent: agent, Scope: scope, Expires: time.Now().Add(time.Minute).Unix()})
		return tok
	}
	get := func(user, tok string) error {
		u := "http://" + addr
		if user != "" {
			u = fmt.Sprintf("http://%s:%s@%s", user, tok, addr)
		}
		resp, err := clientVia(t, u, roots).Get("https://" + target + "/")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}
	if err := get("a1", mint("a1", "fetch")); err != nil {
		t.Fatalf("allowed call failed: %v", err)
	}
	if err := get("a3", mint("a3", "bash")); err != nil {
		t.Fatalf("wildcard call failed: %v", err)
	}
	for name, tc := range map[string][2]string{
		"no credentials":       {"", ""},
		"undeclared scope":     {"a1", mint("a1", "other")},
		"agent without policy": {"a2", mint("a2", "fetch")},
		"user/agent mismatch":  {"a2", mint("a1", "fetch")},
		"unknown agent":        {"zz", mint("zz", "fetch")},
		"wildcard other port":  {"a3", mint("a3", "web")},
	} {
		if err := get(tc[0], tc[1]); err == nil {
			t.Errorf("%s: expected denial", name)
		}
	}
}

func TestPolicyAllows(t *testing.T) {
	p := Policy{"bash": {"*:443"}, "fetch": {"api.x.com:443"}}
	for _, tc := range []struct {
		scope, target string
		want          bool
	}{
		{"bash", "anything.example:443", true},
		{"bash", "1.1.1.1:443", true},
		{"bash", "anything.example:80", false},
		{"bash", "anything.example", false},
		{"fetch", "api.x.com:443", true},
		{"fetch", "api.y.com:443", false},
		{"other", "api.x.com:443", false},
	} {
		if got := p.Allows(tc.scope, tc.target); got != tc.want {
			t.Errorf("Allows(%q, %q) = %v, want %v", tc.scope, tc.target, got, tc.want)
		}
	}
}

// A wildcard scope still cannot reach private addresses.
func TestProxyWildcardRefusesPrivateAddresses(t *testing.T) {
	_, target := upstream(t)
	_, port, _ := strings.Cut(target, ":")
	p := &Proxy{Store: memStore{"a1": {"bash": {"*:" + port}}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Lookup: toLoopback}
	ps := httptest.NewServer(p)
	defer ps.Close()
	tok, _ := Mint(key, Claims{Agent: "a1", Scope: "bash", Expires: time.Now().Add(time.Minute).Unix()})
	u := fmt.Sprintf("http://a1:%s@%s", tok, strings.TrimPrefix(ps.URL, "http://"))
	if _, err := clientVia(t, u, nil).Get("https://" + target + "/"); err == nil {
		t.Fatal("wildcard must not admit localhost")
	}
}

func TestProxyRefusesPrivateAddresses(t *testing.T) {
	_, target := upstream(t)
	p := &Proxy{Store: memStore{"a1": {"fetch": {target}}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Lookup: toLoopback}
	ps := httptest.NewServer(p)
	defer ps.Close()
	tok, _ := Mint(key, Claims{Agent: "a1", Scope: "fetch", Expires: time.Now().Add(time.Minute).Unix()})
	u := fmt.Sprintf("http://a1:%s@%s", tok, strings.TrimPrefix(ps.URL, "http://"))
	if _, err := clientVia(t, u, nil).Get("https://" + target + "/"); err == nil {
		t.Fatal("localhost must be refused even when declared")
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{"1.1.1.1": true, "10.43.0.1": false, "169.254.169.254": false, "192.168.2.225": false, "127.0.0.1": false, "::1": false, "2606:4700::1111": true} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: got %v", addr, got)
		}
	}
}

func TestMock(t *testing.T) {
	m, err := StartMock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Prepare([]string{"api.example.com:443"}, []spec.RecordedHTTP{
		{Method: "GET", URL: "https://api.example.com/v1/thing?id=1", Status: 200, Body: json.RawMessage(`{"name":"thing"}`)},
	})
	pem, _ := os.ReadFile(m.CAFile)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	c := clientVia(t, "http://"+m.ln.Addr().String(), roots)

	resp, err := c.Get("https://api.example.com/v1/thing?id=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"name":"thing"}` {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if len(m.Problems()) != 0 {
		t.Fatalf("problems: %v", m.Problems())
	}
	if resp, err := c.Get("https://api.example.com/v1/other"); err == nil {
		resp.Body.Close()
	}
	if _, err := c.Get("https://evil.example.com/"); err == nil {
		t.Fatal("undeclared host must be refused")
	}
	if p := m.Problems(); len(p) != 2 {
		t.Fatalf("want 2 problems, got %v", p)
	}
}

// factory/test/unit.test.ts mints the same token in TypeScript; both sides
// must agree byte for byte.
func TestTokenVector(t *testing.T) {
	tok, err := Mint([]byte("0123456789abcdef0123456789abcdef"), Claims{Agent: "tap-factory", Scope: "bash", Expires: 1700000000, Nonce: "0011223344556677"})
	if err != nil {
		t.Fatal(err)
	}
	const want = "eyJhIjoidGFwLWZhY3RvcnkiLCJzIjoiYmFzaCIsImUiOjE3MDAwMDAwMDAsIm4iOiIwMDExMjIzMzQ0NTU2Njc3In0.SfXwQHA-X6fxcfowMdsxEsWt-ttRryZRUYPcaPTsdnA"
	if tok != want {
		t.Fatalf("token vector changed:\n got %s\nwant %s", tok, want)
	}
}
