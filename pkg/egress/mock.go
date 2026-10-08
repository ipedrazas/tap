package egress

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ipedrazas/tap/pkg/spec"
)

// Mock is the fixture-time stand-in for the egress proxy. It enforces the
// same allowlist, terminates TLS with a throwaway CA, and answers only from
// recorded responses, so fixtures run with no network at all.
type Mock struct {
	CAFile string
	ln     net.Listener
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey

	mu         sync.Mutex
	allowed    []string
	recordings []spec.RecordedHTTP
	problems   []string
	certs      map[string]*tls.Certificate
}

func StartMock(dir string) (*Mock, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tap fixture CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	ca, _ := x509.ParseCertificate(der)
	caFile := filepath.Join(dir, "tap-fixture-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &Mock{CAFile: caFile, ln: ln, ca: ca, caKey: key, certs: map[string]*tls.Certificate{}}
	go func() { _ = http.Serve(ln, http.HandlerFunc(m.connect)) }()
	return m, nil
}

func (m *Mock) Close() error { return m.ln.Close() }

// Prepare arms the mock for one fixture case.
func (m *Mock) Prepare(allowed []string, recordings []spec.RecordedHTTP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowed, m.recordings, m.problems = allowed, recordings, nil
}

// Problems lists requests that were denied or had no recording.
func (m *Mock) Problems() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.problems)
}

func (m *Mock) problem(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.problems = append(m.problems, fmt.Sprintf(format, args...))
}

// Env routes a tool through the mock and makes it trust the fixture CA.
func (m *Mock) Env() []string {
	env := ProxyEnv("http://" + m.ln.Addr().String())
	return append(env,
		"NODE_EXTRA_CA_CERTS="+m.CAFile,
		"SSL_CERT_FILE="+m.CAFile,
		"REQUESTS_CA_BUNDLE="+m.CAFile,
		"CURL_CA_BUNDLE="+m.CAFile,
	)
}

func (m *Mock) connect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
		return
	}
	target := strings.ToLower(r.Host)
	m.mu.Lock()
	ok := slices.Contains(m.allowed, target)
	m.mu.Unlock()
	if !ok {
		m.problem("egress to %s is not declared for this tool", target)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	hj, _ := w.(http.Hijacker)
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	host, port, _ := net.SplitHostPort(target)
	tlsConn := tls.Server(conn, &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return m.leaf(host) }})
	defer tlsConn.Close()
	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		origin := "https://" + host
		if port != "443" {
			origin += ":" + port
		}
		url := origin + req.URL.RequestURI()
		resp := m.respond(req.Method, url)
		resp.Request = req
		if err := resp.Write(tlsConn); err != nil || req.Close {
			return
		}
	}
}

func (m *Mock) respond(method, url string) *http.Response {
	m.mu.Lock()
	idx := slices.IndexFunc(m.recordings, func(rec spec.RecordedHTTP) bool {
		return strings.EqualFold(rec.Method, method) && rec.URL == url
	})
	var rec spec.RecordedHTTP
	if idx >= 0 {
		rec = m.recordings[idx]
	}
	m.mu.Unlock()
	if idx < 0 {
		m.problem("no recording for %s %s", method, url)
		return textResponse(http.StatusBadGateway, `{"tap_mock":"no recording for this request"}`, "application/json")
	}
	body, ctype := string(rec.Body), "application/json"
	var s string
	if json.Unmarshal(rec.Body, &s) == nil {
		body, ctype = s, "text/plain"
	}
	resp := textResponse(rec.Status, body, ctype)
	for k, v := range rec.Headers {
		resp.Header.Set(k, v)
	}
	return resp
}

func textResponse(status int, body, ctype string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {ctype}},
		Body:          readCloser(body),
		ContentLength: int64(len(body)),
	}
}

func (m *Mock) leaf(host string) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.certs[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	m.certs[host] = c
	return c, nil
}
