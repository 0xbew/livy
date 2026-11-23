//go:build !unix

package amp

import "os/exec"

// detach has no session to start here; the child runs as started.
func detach(*exec.Cmd) {}

// alive cannot ask on this platform, so no marker is ever read as a crash.
func alive(int) bool { return true }
