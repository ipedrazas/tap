package egress

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ipedrazas/tap/pkg/spec"
)

// Policy is one agent's allowlist: scope (tool or "mcp:<server>") -> host:port.
type Policy map[string][]string

// PolicyFor derives an agent's policy from agent.yaml.
func PolicyFor(a *spec.Agent) Policy {
	p := Policy{}
	for _, t := range a.Tools {
		if len(t.Egress) > 0 {
			p[t.Name] = t.Egress
		}
	}
	for _, s := range a.MCP {
		if hp, err := spec.MCPEgress(s.URL); err == nil {
			p["mcp:"+s.Name] = []string{hp}
		}
	}
	return p
}

var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)

type Store interface {
	Key(agent string) ([]byte, error)
	Policy(agent string) (Policy, error)
}

// Proxy is an HTTP CONNECT proxy. Plain-HTTP proxying is refused: tools reach
// the outside over TLS only.
type Proxy struct {
	Store  Store
	Logger *slog.Logger
	// AllowPrivate permits destinations in private address space (tests only).
	AllowPrivate bool
	// Lookup resolves hostnames; nil uses the system resolver.
	Lookup      func(ctx context.Context, host string) ([]netip.Addr, error)
	IdleTimeout time.Duration
	Now         func() time.Time
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && !r.URL.IsAbs() && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT (HTTPS) is supported", http.StatusMethodNotAllowed)
		return
	}
	target := r.Host
	claims, reason := p.authorize(r, target)
	log := p.Logger.With("event", "egress", "target", target, "agent", claims.Agent, "scope", claims.Scope)
	if reason != "" {
		log.Warn("denied", "reason", reason)
		w.Header().Set("Proxy-Authenticate", `Basic realm="tap-egress"`)
		http.Error(w, "egress denied: "+reason, http.StatusForbidden)
		return
	}
	conn, err := p.dial(r.Context(), target)
	if err != nil {
		log.Warn("denied", "reason", err.Error())
		http.Error(w, "egress denied: "+err.Error(), http.StatusForbidden)
		return
	}
	defer conn.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	log.Info("allowed")
	start := time.Now()
	up, down := splice(conn, client, rw.Reader, p.idle())
	log.Info("closed", "bytes_up", up, "bytes_down", down, "duration_ms", time.Since(start).Milliseconds())
}

// authorize returns the claims and a denial reason ("" when allowed).
func (p *Proxy) authorize(r *http.Request, target string) (Claims, string) {
	user, token, ok := proxyAuth(r)
	if !ok {
		return Claims{}, "missing proxy credentials"
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	c, err := Verify(token, p.Store.Key, now)
	if err != nil {
		return c, err.Error()
	}
	if c.Agent != user {
		return c, "credential agent mismatch"
	}
	pol, err := p.Store.Policy(c.Agent)
	if err != nil {
		return c, err.Error()
	}
	if !slices.Contains(pol[c.Scope], strings.ToLower(target)) {
		return c, fmt.Sprintf("%s is not declared for %s", target, c.Scope)
	}
	return c, ""
}

func proxyAuth(r *http.Request) (user, pass string, ok bool) {
	h := r.Header.Get("Proxy-Authorization")
	enc, found := strings.CutPrefix(h, "Basic ")
	if !found {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(raw), ":")
}

// dial resolves the host and refuses private, loopback and link-local
// addresses, so a declared hostname cannot be pointed at the cluster.
func (p *Proxy) dial(ctx context.Context, target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("bad target")
	}
	lookup := p.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("cannot resolve %s", host)
	}
	var d net.Dialer
	var lastErr error
	for _, a := range addrs {
		a = a.Unmap()
		if !p.AllowPrivate && !public(a) {
			lastErr = fmt.Errorf("%s resolves to non-public address %s", host, a)
			continue
		}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = fmt.Errorf("connect %s: %v", host, err)
	}
	return nil, lastErr
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func public(a netip.Addr) bool {
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return a.IsValid() && !a.IsUnspecified()
}

func (p *Proxy) idle() time.Duration {
	if p.IdleTimeout > 0 {
		return p.IdleTimeout
	}
	return 2 * time.Minute
}

// splice copies both ways until either side closes or goes idle.
func splice(upstream, client net.Conn, buffered io.Reader, idle time.Duration) (up, down int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn, r io.Reader, n *int64) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			k, err := r.Read(buf)
			if k > 0 {
				*n += int64(k)
				if _, werr := dst.Write(buf[:k]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		// Unblock the other direction.
		_ = dst.SetReadDeadline(time.Now())
		_ = src.SetReadDeadline(time.Now())
	}
	go cp(upstream, client, buffered, &up)
	go cp(client, upstream, upstream, &down)
	wg.Wait()
	return up, down
}
