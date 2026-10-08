package bundle

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/ipedrazas/tap/pkg/spec"
)

// Repository is where an agent's bundles live: <registry>/agents/<name>.
func Repository(p *spec.Platform, agent string) string {
	return p.Registry + "/agents/" + agent
}

// Push uploads img as <repo>:<version>. Version tags are immutable: pushing
// different content under an existing version fails. With dev set, the image
// goes to the moving tag "dev" instead, for the inner loop.
func Push(img v1.Image, repo, version string, dev bool) (name.Digest, error) {
	digest, err := img.Digest()
	if err != nil {
		return name.Digest{}, err
	}
	tagName := version
	if dev {
		tagName = "dev"
	}
	tag, err := name.NewTag(repo + ":" + tagName)
	if err != nil {
		return name.Digest{}, err
	}
	opts := []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}
	if !dev {
		existing, err := remote.Head(tag, opts...)
		var terr *transport.Error
		switch {
		case err == nil && existing.Digest != digest:
			return name.Digest{}, fmt.Errorf("%s already points at %s; bump metadata.version (this build is %s)", tag, existing.Digest, digest)
		case err != nil && !(errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound):
			return name.Digest{}, fmt.Errorf("checking %s: %w", tag, err)
		}
	}
	if err := remote.Write(tag, img, opts...); err != nil {
		return name.Digest{}, err
	}
	return tag.Context().Digest(digest.String()), nil
}

// Pull fetches the agent.yaml of a pushed bundle, for diffs against what is
// deployed.
func PullAgentYAML(ref string) ([]byte, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(r, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, err
	}
	return readFile(img, "runner/agent.yaml")
}
