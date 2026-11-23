package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrHeld is a lock another process held for the whole wait.
var ErrHeld = errors.New("lock held by another process")

// WithLock runs fn while holding dir's ledger lock, the one every append holds, waiting up
// to wait for it. acquired is false when fn did not run: with a nil error when another
// process held the lock for the whole wait, otherwise with the error that kept the lock file
// from opening.
func WithLock(dir string, wait time.Duration, fn func() error) (acquired bool, err error) {
	release, err := Lock(filepath.Join(dir, LockName), wait)
	if errors.Is(err, ErrHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer release()
	return true, fn()
}

// Lock takes an exclusive lock on the file at path (created if missing), waiting up to wait
// (0 tries once). release frees it. ErrHeld means another process holds it; any other
// error means the lock file itself could not be opened.
func Lock(path string, wait time.Duration) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if !acquire(f, wait) {
		f.Close()
		return nil, ErrHeld
	}
	return func() { unlock(f); f.Close() }, nil
}
