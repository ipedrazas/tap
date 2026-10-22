package egress

import (
	"cmp"
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
// "*:<port>" allows any host on that port; dial still refuses non-public
// addresses. Rule 4 keeps "*" out of every agent.yaml, so only hand-written
// policies (the factory's) contain it.
type Policy map[string][]string

// Allows reports whether scope may reach target (host:port, lowercase).
func (p Policy) Allows(scope, target string) bool {
	allowed := p[scope]
	if slices.Contains(allowed, target) {
		return true
	}
	_, port, err := net.SplitHostPort(target)
	return err == nil && slices.Contains(allowed, "*:"+port)
}

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

// SessionsScope is casa-collector's scope, for uploading session traces.
// Tool names cannot contain ':', so no tool can mint it, and the runner
// never does.
const SessionsScope = "collector:sessions"

// PolicyForPlatform is the agent's policy plus the platform's own scopes:
// the session trace bucket, when export is on.
func PolicyForPlatform(a *spec.Agent, p *spec.Platform) (Policy, error) {
	pol := PolicyFor(a)
	if p.Sessions.Enabled() {
		hp, err := p.Sessions.Egress()
		if err != nil {
			return nil, err
		}
		pol[SessionsScope] = []string{hp}
	}
	return pol, nil
}

// An agent name (schema: at most 40 characters), or its preview's <name>-dev.
var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,42}[a-z0-9]$`)

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
	// MaxTunnel bounds a tunnel's life (default 15m), so it can't outlive
	// its credential or a policy change for long.
	MaxTunnel time.Duration
	// MaxTunnels and MaxPerAgent cap open tunnels in all and per agent
	// (defaults 512 and 64): one busy tool can't use up the proxy.
	MaxTunnels, MaxPerAgent int
	Now                     func() time.Time

	mu       sync.Mutex
	open     int
	perAgent map[string]int
}

// enter takes a tunnel slot for agent ("" checks only the total).
func (p *Proxy) enter(agent string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open >= cmp.Or(p.MaxTunnels, 512) || (agent != "" && p.perAgent[agent] >= cmp.Or(p.MaxPerAgent, 64)) {
		return false
	}
	if agent == "" {
		return true
	}
	if p.perAgent == nil {
		p.perAgent = map[string]int{}
	}
	p.open++
	p.perAgent[agent]++
	return true
}

func (p *Proxy) leave(agent string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.open--
	if p.perAgent[agent]--; p.perAgent[agent] <= 0 {
		delete(p.perAgent, agent)
	}
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
	if !p.enter("") {
		http.Error(w, "egress proxy busy", http.StatusServiceUnavailable)
		return
	}
	target := r.Host
	claims, reason := p.authorize(r, target)
	log := p.Logger.With("event", "egress", "target", target, "agent", claims.Agent, "scope", claims.Scope, "remote", r.RemoteAddr)
	if reason != "" {
		log.Warn("denied", "reason", reason)
		w.Header().Set("Proxy-Authenticate", `Basic realm="tap-egress"`)
		http.Error(w, "egress denied: "+reason, http.StatusForbidden)
		return
	}
	if !p.enter(claims.Agent) {
		log.Warn("denied", "reason", "too many tunnels")
		http.Error(w, "egress proxy busy: too many tunnels for "+claims.Agent, http.StatusServiceUnavailable)
		return
	}
	defer p.leave(claims.Agent)
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
	log.Info("allowed", "ip", conn.RemoteAddr().String())
	start := time.Now()
	up, down := splice(conn, client, rw.Reader, p.idle(), time.Now().Add(cmp.Or(p.MaxTunnel, 15*time.Minute)))
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
	// Only agents with a policy have keys worth looking up: a token naming
	// anything else is refused before it costs an OpenBao read.
	keyFor := func(agent string) ([]byte, error) {
		if _, err := p.Store.Policy(agent); err != nil {
			return nil, err
		}
		return p.Store.Key(agent)
	}
	c, err := Verify(token, keyFor, now)
	identity := c.Agent
	if err != nil {
		// A preview (<name>-dev, see spec.PreviewSuffix) runs the bundle
		// unchanged, so its runner signs as <name>, but with the preview's own
		// key. The key decides who is calling, and the preview gets its own
		// policy: neither can use the other's allowlist.
		pc, perr := Verify(token, func(agent string) ([]byte, error) { return keyFor(agent + spec.PreviewSuffix) }, now)
		if perr != nil {
			return c, err.Error()
		}
		c, identity = pc, pc.Agent+spec.PreviewSuffix
	}
	if c.Agent != user {
		return c, "credential agent mismatch"
	}
	c.Agent = identity
	pol, err := p.Store.Policy(identity)
	if err != nil {
		return c, err.Error()
	}
	if !pol.Allows(c.Scope, strings.ToLower(target)) {
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

// splice copies both ways until either side closes, goes idle (no read, or
// a write that doesn't complete, within idle) or the tunnel reaches end.
func splice(upstream, client net.Conn, buffered io.Reader, idle time.Duration, end time.Time) (up, down int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	deadline := func() time.Time {
		if d := time.Now().Add(idle); d.Before(end) {
			return d
		}
		return end
	}
	cp := func(dst, src net.Conn, r io.Reader, n *int64) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(deadline())
			k, err := r.Read(buf)
			if k > 0 {
				*n += int64(k)
				_ = dst.SetWriteDeadline(deadline())
				if _, werr := dst.Write(buf[:k]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		// Unblock the other direction, reading or writing.
		_ = dst.SetDeadline(time.Now())
		_ = src.SetDeadline(time.Now())
	}
	go cp(upstream, client, buffered, &up)
	go cp(client, upstream, upstream, &down)
	wg.Wait()
	return up, down
}
