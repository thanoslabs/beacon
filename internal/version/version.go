// Package version reports which build of Beacon is running.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is the release version as a bare X.Y.Z string. Release builds set
// it with:
//
//	go build -ldflags "-X github.com/thanoslabs/beacon/internal/version.Version=1.2.3"
//
// Builds that do not set it report "dev".
var Version = "dev"

// Info describes a build.
type Info struct {
	Version   string
	Commit    string
	Modified  bool
	GoVersion string
	Platform  string
}

// Get returns the build information. The commit comes from the VCS details
// that the Go toolchain embeds, so it needs no build flag.
func Get() Info {
	info := Info{
		Version:   Version,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	return info
}

// String formats the build information on one line.
func (i Info) String() string {
	commit := i.Commit
	if commit == "" {
		commit = "unknown"
	} else if len(commit) > 12 {
		commit = commit[:12]
	}
	if i.Modified {
		commit += "-modified"
	}
	return fmt.Sprintf("beacon %s (commit %s, %s, %s)", i.Version, commit, i.GoVersion, i.Platform)
}
