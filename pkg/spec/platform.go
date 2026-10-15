package spec

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Platform is platform.yaml at the repo root.
type Platform struct {
	Registry        string          `json:"registry"`
	Domain          string          `json:"domain"`
	NamespacePrefix string          `json:"namespacePrefix"`
	RuntimeClass    string          `json:"runtimeClass"`
	Gateway         PlatformGateway `json:"gateway"`
	Model           PlatformModel   `json:"model"`
	EgressProxy     struct {
		Address string `json:"address"`
		Image   string `json:"image"`
	} `json:"egressProxy"`
	ToolUIDBase int `json:"toolUIDBase"`
	Harness     struct {
		Image     string    `json:"image"`
		Resources Resources `json:"resources"`
	} `json:"harness"`
	Runners []RunnerImage `json:"runners"`
	Signing Signing       `json:"signing"`
	Secrets Secrets       `json:"secrets"`
	// Sessions, when Endpoint is set, gives every agent a casa-collector
	// sidecar that uploads its session traces (casa trace documents) to this
	// bucket.
	Sessions Sessions `json:"sessions"`
	// Hooks, when PublicURL is set, gives every agent a trigger port that
	// tap-hooks (in the gateway namespace) may call, and lets owners add
	// hooks in the console.
	Hooks Hooks `json:"hooks"`
	// Teams maps an agent owner (metadata.owner) to the emails that may
	// change that agent's settings in the console.
	Teams map[string][]string `json:"teams,omitempty"`
}

// Sessions is the S3-compatible bucket (Tigris) session traces go to. The
// harness writes traces to a volume; casa-collector (CollectorImage) uploads
// them through the egress proxy. The write key lives in
// tap-system/sessions-credentials and is mounted into the collector only.
type Sessions struct {
	Endpoint       string `json:"endpoint"`
	Bucket         string `json:"bucket"`
	Region         string `json:"region"`
	CollectorImage string `json:"collectorImage"`
}

// Enabled reports whether session traces are exported.
func (s Sessions) Enabled() bool { return s.Endpoint != "" }

// Egress is the host:port the collector dials for uploads.
func (s Sessions) Egress() (string, error) {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("platform.yaml sessions.endpoint %q must be an https URL", s.Endpoint)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return strings.ToLower(u.Hostname()) + ":" + port, nil
}

// Hooks is where external systems send hook deliveries: tap-hooks serves
// <PublicURL>/h/<agent>/<hook>.
type Hooks struct {
	PublicURL string `json:"publicURL"`
}

// Enabled reports whether agents accept hook triggers.
func (h Hooks) Enabled() bool { return h.PublicURL != "" }

// URL is the public URL of one hook.
func (h Hooks) URL(agent, hook string) string {
	return strings.TrimRight(h.PublicURL, "/") + "/h/" + agent + "/" + hook
}

// Secrets says where External Secrets Operator reads agent secrets from.
type Secrets struct {
	// Server is OpenBao's TLS listener.
	Server string `json:"server"`
	// KVMount is the KV v2 mount; Prefix the path tap owns inside it.
	KVMount string `json:"kvMount"`
	Prefix  string `json:"prefix"`
	// AuthMount and Role are the Kubernetes auth method and its role.
	AuthMount string `json:"authMount"`
	Role      string `json:"role"`
	// Audience is required on the service account tokens ESO presents.
	Audience string `json:"audience"`
	// SharedPrefix holds the platform secrets every agent namespace reads
	// (model key, trigger token, sessions write key), pushed there from
	// tap-system. Outside Prefix, so the console and seeder can't write them.
	SharedPrefix string `json:"sharedPrefix"`
	// EgressPrefix holds each agent's egress key at <EgressPrefix>/<namespace>,
	// pushed from the agent's namespace and read by the egress proxy.
	EgressPrefix string `json:"egressPrefix"`
}

// Complete reports whether every field render needs is set.
func (s Secrets) Complete() bool {
	return s.Server != "" && s.KVMount != "" && s.Prefix != "" && s.AuthMount != "" && s.Role != "" && s.Audience != "" &&
		s.SharedPrefix != "" && s.EgressPrefix != ""
}

// Signing configures bundle and image signatures and their admission check.
type Signing struct {
	// PublicKey is the pipeline's cosign public key (PEM).
	PublicKey string `json:"publicKey"`
	// TestedPublicKey verifies the second signature that marks a bundle
	// whose fixtures passed; only the fixture pipeline holds its private key.
	TestedPublicKey string `json:"testedPublicKey"`
	// PredicateType is the in-toto predicate type of bundle attestations.
	PredicateType string `json:"predicateType"`
	// Admission is "audit" (report violations) or "enforce" (reject pods).
	Admission string `json:"admission"`
}

type PlatformGateway struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Listener  string `json:"listener"`
	// PodNamespace is where Envoy Gateway runs the gateway's proxy pods.
	PodNamespace string `json:"podNamespace"`
}

type PlatformModel struct {
	BaseURL       string   `json:"baseURL"`
	Routes        []string `json:"routes"`
	APIKeyHeader  string   `json:"apiKeyHeader"`
	NetworkPolicy struct {
		Namespace string            `json:"namespace"`
		Port      int               `json:"port"`
		PodLabels map[string]string `json:"podLabels"`
	} `json:"networkPolicy"`
}

type RunnerImage struct {
	Name         string   `json:"name"`
	Image        string   `json:"image"`
	Interpreters []string `json:"interpreters"`
}

func LoadPlatform(path string) (*Platform, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Platform
	if err := yaml.UnmarshalStrict(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// Runner returns the curated runner whose pinned image equals ref.
func (p *Platform) Runner(ref string) (RunnerImage, bool) {
	i := slices.IndexFunc(p.Runners, func(r RunnerImage) bool { return r.Image != "" && r.Image == ref })
	if i < 0 {
		return RunnerImage{}, false
	}
	return p.Runners[i], true
}

// SecretPath is where agent a's secret name lives in OpenBao, relative to
// the KV mount: <prefix>/<namespace>/<key>.
func (p *Platform) SecretPath(a *Agent, name string) (string, error) {
	sec, ok := a.Secrets[name]
	if !ok {
		return "", fmt.Errorf("%s declares no secret %s", a.Metadata.Name, name)
	}
	key, err := sec.Key(a.Metadata.Name)
	if err != nil {
		return "", err
	}
	return p.Secrets.Prefix + "/" + p.NamespacePrefix + a.Metadata.Name + "/" + key, nil
}
