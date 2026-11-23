// Command render prints the Amp plugin `livy install` would write for a livy binary and data
// folder on another machine, such as an orb: build.sh bundles it for hosted orbs.
package main

import (
	"flag"
	"fmt"
	"os"
	"path"

	"github.com/0xbew/livy/internal/install"
)

func main() {
	bin := flag.String("bin", "", "absolute path of the livy binary on the target machine")
	data := flag.String("data", "", "absolute path of Livy's data folder on the target machine")
	flag.Parse()
	if !path.IsAbs(*bin) || !path.IsAbs(*data) {
		fmt.Fprintln(os.Stderr, "usage: render -bin /abs/livy -data /abs/data-folder")
		os.Exit(2)
	}
	if _, err := os.Stdout.Write(install.AmpPlugin(*bin, *data)); err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
}
