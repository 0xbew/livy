// Package buildinfo says which build of Livy is running, from the stamp the Go toolchain
// puts in a binary: the commit it was built from, +dirty for uncommitted changes.
package buildinfo

import "runtime/debug"

// Info is what a binary knows about how it was built. Rev is the 12-character commit with
// +dirty appended for uncommitted changes, or the module version for a `go install …@vX`
// build, or "(devel)" when nothing says which commit.
type Info struct {
	Rev       string
	Time      string // the commit's time, "" when unstamped
	GoVersion string
}

// Read is this binary's build, and false when the binary carries no build info.
func Read() (Info, bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Info{}, false
	}
	var rev, at, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev == "" {
		rev = bi.Main.Version
	}
	return Info{Rev: rev + dirty, Time: at, GoVersion: bi.GoVersion}, true
}
