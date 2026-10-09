// Package version holds the build's version, set at link time:
//
//	go build -ldflags "-X github.com/ubixsys/ubixshepherd/internal/version.Version=v0.1.0"
//
// A build that is not stamped (go install ...@v0.1.0) reports the module version Go
// recorded in the binary instead, and "dev" only when there is none.
package version

import "runtime/debug"

// Version is the build's version: the stamped one, else the module's, else "dev".
var Version = "dev"

func init() { Version = resolve(Version, debug.ReadBuildInfo) }

// resolve is the version to report: stamped when the build set one, else the main
// module's version from read (what go install records), else "dev".
func resolve(stamped string, read func() (*debug.BuildInfo, bool)) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	if bi, ok := read(); ok && bi != nil {
		// "(devel)" is what Go records when it knows no version.
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
