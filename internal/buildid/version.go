package buildid

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// version is the release version. A release build sets it with
//
//	-ldflags "-X github.com/nicodes/stavlos/internal/buildid.version=v0.1.0"
//
// otherwise it comes from the module version Go embeds (a tag for
// go install …@v0.1.0, a pseudo-version for a local build).
var version string

// Info describes the running binary for stavlos --version.
type Info struct {
	Version   string // v0.1.0, a pseudo-version, or "devel"
	Revision  string // git commit the binary was built from, "" when unknown
	Time      string // that commit's time (RFC 3339), "" when unknown
	Modified  bool   // the working tree had uncommitted changes
	GoVersion string
	Platform  string // GOOS/GOARCH
	ID        string // the executable hash the daemon restart check uses
}

// Read collects Info from the version stamp and the embedded build info.
func Read() Info {
	info := Info{Version: version, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, ID: ID()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Revision = s.Value
			case "vcs.time":
				info.Time = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if info.Version == "" || info.Version == "(devel)" {
		info.Version = "devel"
	}
	return info
}

// String is the --version text: the version, then the commit, the
// toolchain and the build id, one per line.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stavlos %s\n", i.Version)
	if i.Revision != "" {
		commit := i.Revision
		if len(commit) > 12 {
			commit = commit[:12]
		}
		if i.Time != "" {
			commit += ", " + i.Time
		}
		if i.Modified {
			commit += ", with uncommitted changes"
		}
		fmt.Fprintf(&b, "commit  %s\n", commit)
	}
	fmt.Fprintf(&b, "go      %s %s\n", i.GoVersion, i.Platform)
	fmt.Fprintf(&b, "build   %s\n", i.ID)
	return b.String()
}
