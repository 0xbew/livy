package main

import (
	"fmt"
	"io"

	"github.com/0xbew/livy/internal/buildinfo"
)

// printVersion says which build this binary is, so two machines can be compared: the commit
// it was built from (with +dirty for uncommitted changes), the commit's time and the Go
// version, as the Go toolchain stamps them into a build made from a git checkout. A
// `go install …@vX` build reports its module version instead, and a build with no VCS
// stamp reports "(devel)", which cannot tell two machines apart.
func printVersion(w io.Writer) int {
	b, ok := buildinfo.Read()
	if !ok {
		fmt.Fprintln(w, "livy (no build info)")
		return 0
	}
	out := "livy " + b.Rev
	if b.Time != "" {
		out += " " + b.Time
	}
	fmt.Fprintln(w, out+" "+b.GoVersion)
	return 0
}
