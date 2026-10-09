// Package version reports what a binary was built from. Release builds set
// the variables with -ldflags; otherwise the VCS data Go embeds is used.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set with: -ldflags "-X github.com/ipedrazas/tap/pkg/version.Version=... -X ...Commit=... -X ...Date=..."
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"buildDate"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

func Get() Info {
	i := Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.time":
				if i.Date == "" {
					i.Date = s.Value + " (commit)"
				}
			case "vcs.modified":
				i.Dirty = s.Value == "true"
			}
		}
	}
	// Release builds mark a dirty tree with a "-dirty" commit suffix.
	if c, ok := strings.CutSuffix(i.Commit, "-dirty"); ok {
		i.Commit, i.Dirty = c, true
	}
	if i.Commit == "" {
		i.Commit = "unknown"
	}
	if i.Date == "" {
		i.Date = "unknown"
	}
	return i
}

func (i Info) Short() string {
	c := i.Commit
	if len(c) > 12 {
		c = c[:12]
	}
	if i.Dirty {
		c += "-dirty"
	}
	return fmt.Sprintf("%s (%s, built %s)", i.Version, c, i.Date)
}

func (i Info) String() string {
	dirty := ""
	if i.Dirty {
		dirty = " (dirty)"
	}
	return fmt.Sprintf("version:  %s\ncommit:   %s%s\nbuilt:    %s\ngo:       %s\nplatform: %s", i.Version, i.Commit, dirty, i.Date, i.GoVersion, i.Platform)
}
