//go:build unix

package install

import "syscall"

// alive says a process with this pid exists.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
