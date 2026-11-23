//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
)

// lock takes the named lock without waiting: a second pass leaves the work to the first.
func lock(dir, name string) (func(), bool) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true
}
