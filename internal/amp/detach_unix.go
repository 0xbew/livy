//go:build unix

package amp

import (
	"errors"
	"os/exec"
	"syscall"
)

// detach puts cmd in a session of its own, so it outlives the plugin and Amp.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// alive says a process with this pid exists; one owned by another user counts.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
