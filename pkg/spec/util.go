package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ReadFile reads rel inside dir. It refuses anything but a regular file, and
// any path that leaves dir, through symlinks too: a bundle carries only what
// is in its directory (a symlinked prompt would otherwise ship, say,
// ~/.kube/config from the machine that builds it).
func ReadFile(dir, rel string) ([]byte, error) {
	if err := Regular(dir, rel); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(rel)
}

// Regular reports whether rel is a regular file inside dir (not a symlink).
func Regular(dir, rel string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	fi, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: only regular files are allowed in bundles", filepath.ToSlash(rel))
	}
	return nil
}

// A secret becomes an environment variable of the same name, and its value
// is write-only and outside the bundle's digest and review. So a secret may
// not take a name the runner sets, or one that changes what a tool's
// interpreter loads or where it connects: NODE_OPTIONS=--import=data:...
// would be unreviewed code.
var (
	reservedEnv = []string{
		"PATH", "HOME", "TMPDIR", "LANG", "SHELL", "USER", "PWD", "IFS", "ENV", "BASH_ENV", "CDPATH",
		"GCONV_PATH", "JAVA_TOOL_OPTIONS", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB",
		"GIT_SSH", "GIT_SSH_COMMAND", "GIT_EXEC_PATH", "GIT_ASKPASS", "SSH_ASKPASS",
		"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
	}
	reservedEnvPrefixes = []string{"NODE_", "NPM_", "PYTHON", "PIP_", "UV_", "LD_", "DYLD_", "LC_", "TAP_", "SSL_", "GIT_CONFIG", "MALLOC_", "GLIBC_"}
)

// ReservedEnv reports whether a secret may not be named name.
func ReservedEnv(name string) bool {
	if slices.Contains(reservedEnv, name) || strings.HasSuffix(name, "_PROXY") {
		return true
	}
	return slices.ContainsFunc(reservedEnvPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

// MCPEgress derives the host:port an MCP server URL needs.
func MCPEgress(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("%q: MCP servers must use https", raw)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname() + ":" + port, nil
}

// SpecHash is the sha256 of the canonical agent.yaml, recorded on the bundle
// image for provenance.
func (b *Bundle) SpecHash() string {
	sum := sha256.Sum256(canonical(b.Raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeAny(raw []byte) (any, error) {
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}

// encoding/json writes map keys in sorted order, which is all canonical needs.
func marshalSorted(v any) ([]byte, error) { return json.Marshal(v) }

// Key returns the secret's key within the agent's own OpenBao path:
// vault://<agent>/<key> gives <key>.
func (s Secret) Key(agent string) (string, error) {
	rest, ok := strings.CutPrefix(s.From, "vault://"+agent+"/")
	if !ok || rest == "" {
		return "", fmt.Errorf("%q must be vault://%s/<key>: an agent can only read its own secrets", s.From, agent)
	}
	return rest, nil
}
