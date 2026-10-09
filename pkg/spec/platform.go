package spec

import (
	"fmt"
	"os"
	"slices"

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
