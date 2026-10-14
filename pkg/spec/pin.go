package spec

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

var digestRef = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9./_-]+@sha256:[a-f0-9]{64}$`)

// PinImage sets harness.image (name "harness"), egressProxy.image,
// sessions.collectorImage (name "casa-collector") or runners[name].image in
// platform.yaml, keeping comments and layout.
func PinImage(path, name, ref string) error {
	if !digestRef.MatchString(ref) {
		return fmt.Errorf("%q is not a digest reference", ref)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return err
	}
	root := doc.Content[0]
	var target *yamlv3.Node
	field := "image"
	if name == "harness" {
		target = mapValue(root, "harness")
	} else if name == "egress-proxy" {
		target = mapValue(root, "egressProxy")
	} else if name == "casa-collector" {
		target, field = mapValue(root, "sessions"), "collectorImage"
	} else if runners := mapValue(root, "runners"); runners != nil {
		for _, r := range runners.Content {
			if n := mapValue(r, "name"); n != nil && n.Value == name {
				target = r
			}
		}
	}
	if target == nil {
		return fmt.Errorf("%s: no %q entry", path, name)
	}
	img := mapValue(target, field)
	if img == nil {
		return fmt.Errorf("%s: %q has no %s field", path, name, field)
	}
	img.Value, img.Style = ref, 0
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// signingFields maps a key's role to its platform.yaml signing field.
var signingFields = map[string]string{"build": "publicKey", "tested": "testedPublicKey"}

// SigningKey returns the PEM public key admission trusts for role (build or
// tested).
func (p *Platform) SigningKey(role string) (string, error) {
	var pem string
	switch role {
	case "build":
		pem = p.Signing.PublicKey
	case "tested":
		pem = p.Signing.TestedPublicKey
	default:
		return "", fmt.Errorf("signing key role must be build or tested, not %q", role)
	}
	if !strings.Contains(pem, "BEGIN PUBLIC KEY") {
		return "", fmt.Errorf("platform.yaml signing.%s is not a PEM public key", signingFields[role])
	}
	return strings.TrimSpace(pem) + "\n", nil
}

// PinSigningKey sets signing.publicKey (build) or signing.testedPublicKey
// (tested) in platform.yaml, keeping comments and layout.
func PinSigningKey(path, role, pem string) error {
	field, ok := signingFields[role]
	if !ok {
		return fmt.Errorf("signing key role must be build or tested, not %q", role)
	}
	pem = strings.TrimSpace(pem)
	if !strings.HasPrefix(pem, "-----BEGIN PUBLIC KEY-----") || !strings.HasSuffix(pem, "-----END PUBLIC KEY-----") {
		return fmt.Errorf("not a PEM public key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return err
	}
	v := mapValue(mapValue(doc.Content[0], "signing"), field)
	if v == nil {
		return fmt.Errorf("%s: no signing.%s", path, field)
	}
	v.Value, v.Style = pem+"\n", yamlv3.LiteralStyle
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func mapValue(m *yamlv3.Node, key string) *yamlv3.Node {
	if m == nil || m.Kind != yamlv3.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// BumpRunner rewrites runner.image in an agent.yaml to the curated image with
// the same repository. It edits the line in place so formatting is kept.
func BumpRunner(agentPath string, p *Platform) (from, to string, err error) {
	data, err := os.ReadFile(agentPath)
	if err != nil {
		return "", "", err
	}
	a, _, err := ParseAgent(data)
	if err != nil {
		return "", "", err
	}
	from = a.Runner.Image
	repo, _, _ := strings.Cut(from, "@")
	for _, r := range p.Runners {
		if r.Image != "" && strings.HasPrefix(r.Image, repo+"@") {
			to = r.Image
		}
	}
	if to == "" {
		return from, "", fmt.Errorf("no curated runner image for %s in platform.yaml", repo)
	}
	if to == from {
		return from, to, nil
	}
	return from, to, os.WriteFile(agentPath, bytes.Replace(data, []byte(from), []byte(to), 1), 0o644)
}
