package egress

import (
	"io"
	"strings"
)

func readCloser(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
