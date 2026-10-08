package bundle

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// readFile returns one file from the image's flattened filesystem.
func readFile(img v1.Image, name string) ([]byte, error) {
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in bundle", name)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == name {
			return io.ReadAll(io.LimitReader(tr, 1<<20))
		}
	}
}
